// Package upstream streams Command Code /alpha/generate events.
//
// Stream retries transient failures that arrive before any content event — both
// retryable HTTP statuses (429 / 5xx) and isRetryable stream errors — mirroring
// the upstream extension's "never retry once visible content was emitted" rule.
// A non-200 HTTP response that isn't retried surfaces as an *UpstreamError so the
// route can pick a real HTTP status before committing a response.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/liwei/commandcode-proxy-go/internal/translate"
)

var contentEventTypes = map[string]bool{"text-delta": true, "reasoning-delta": true, "tool-call": true}
var retryableStatus = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

// decisiveTypes are the events that let a route choose an HTTP status.
var decisiveTypes = map[string]bool{
	"text-delta": true, "reasoning-delta": true, "tool-call": true, "finish": true, "error": true,
}

// errRetry is an internal signal to re-issue the whole request.
var errRetry = errors.New("retry")

// UpstreamError is a non-retryable upstream failure surfaced as an HTTP status.
type UpstreamError struct {
	Status  int
	Message string
	Code    string
}

func (e *UpstreamError) Error() string { return e.Message }

// Stream is a pull-based iterator over Command Code stream events.
type Stream struct {
	ctx        context.Context
	client     *http.Client
	url        string
	body       []byte
	headers    map[string]string
	maxRetries int
	sleep      func(time.Duration) // overridable in tests

	resp     *http.Response
	reader   *bufio.Reader
	attempt  int
	produced bool
	done     bool
}

// NewStream prepares a stream for the given request body (not yet opened).
func NewStream(ctx context.Context, client *http.Client, url string, body map[string]any, headers map[string]string, maxRetries int) *Stream {
	raw, _ := json.Marshal(body)
	return &Stream{
		ctx: ctx, client: client, url: url, body: raw, headers: headers,
		maxRetries: maxRetries, sleep: time.Sleep,
	}
}

// Next returns the next event, io.EOF when drained, or an *UpstreamError (only
// before any content has been emitted).
func (s *Stream) Next() (translate.Event, error) {
	if s.done {
		return nil, io.EOF
	}
	for {
		if s.reader == nil {
			switch err := s.open(); {
			case err == errRetry:
				s.backoff()
				continue
			case err != nil:
				s.done = true
				return nil, err
			}
		}

		line, readErr := s.reader.ReadString('\n')
		if readErr != nil {
			// End of this response (EOF or read error): parse any trailing line.
			s.closeResp()
			s.done = true
			if ev := translate.ParseStreamEventLine(line); ev != nil {
				return ev, nil
			}
			return nil, io.EOF
		}

		ev := translate.ParseStreamEventLine(line)
		if ev == nil {
			continue
		}
		etype, _ := ev["type"].(string)
		if contentEventTypes[etype] {
			s.produced = true
		}
		if etype == "error" {
			e, _ := ev["error"].(map[string]any)
			if isRetryable(e) && !s.produced && s.attempt < s.maxRetries {
				s.closeResp()
				s.backoff()
				continue
			}
			s.closeResp()
			s.done = true
			return ev, nil
		}
		return ev, nil
	}
}

// Close releases the underlying response, if open.
func (s *Stream) Close() { s.closeResp() }

func (s *Stream) open() error {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.url, bytes.NewReader(s.body))
	if err != nil {
		return &UpstreamError{Status: 502, Message: err.Error()}
	}
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return &UpstreamError{Status: 502, Message: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		msg, code := extractError(string(raw))
		if retryableStatus[resp.StatusCode] && !s.produced && s.attempt < s.maxRetries {
			return errRetry
		}
		return &UpstreamError{Status: resp.StatusCode, Message: msg, Code: code}
	}
	s.resp = resp
	s.reader = bufio.NewReader(resp.Body)
	return nil
}

func (s *Stream) closeResp() {
	if s.resp != nil {
		s.resp.Body.Close()
		s.resp = nil
		s.reader = nil
	}
}

func (s *Stream) backoff() {
	s.attempt++
	secs := math.Min(0.5*math.Pow(2, float64(s.attempt)), 8.0)
	s.sleep(time.Duration(secs * float64(time.Second)))
}

// AdvanceToDecisive drains leading non-decisive events and returns the first
// content / finish / error event, or (nil, nil) if the stream ends first. An
// *UpstreamError (non-200 at open) is returned as the error.
func AdvanceToDecisive(s *Stream) (translate.Event, error) {
	for {
		ev, err := s.Next()
		if err != nil {
			if err == io.EOF {
				return nil, nil
			}
			return nil, err
		}
		if etype, _ := ev["type"].(string); decisiveTypes[etype] {
			return ev, nil
		}
	}
}

func isRetryable(e map[string]any) bool {
	v, _ := e["isRetryable"].(bool)
	return v
}

// extractError pulls a clean (message, code) from a Command Code error body.
// Command Code non-200 responses look like
// {"success": false, "error": {"code", "status", "message", "docs"}}. Falls back
// to the raw (truncated) text when it isn't that shape.
func extractError(text string) (string, string) {
	trunc := text
	if len(trunc) > 500 {
		trunc = trunc[:500]
	}
	fallback := func() string {
		if s := strings.TrimSpace(trunc); s != "" {
			return s
		}
		return "upstream error"
	}

	var data any
	if json.Unmarshal([]byte(text), &data) != nil {
		return fallback(), ""
	}
	m, ok := data.(map[string]any)
	if !ok {
		return fallback(), ""
	}
	switch e := m["error"].(type) {
	case map[string]any:
		msg, _ := e["message"].(string)
		if msg == "" {
			if msg = trunc; msg == "" {
				msg = "upstream error"
			}
		}
		code, _ := e["code"].(string)
		return msg, code
	case string:
		return e, ""
	default:
		return fallback(), ""
	}
}
