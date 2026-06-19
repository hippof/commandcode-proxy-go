package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/liwei/commandcode-proxy-go/internal/translate"
)

func newTestStream(t *testing.T, h http.HandlerFunc, maxRetries int) (*Stream, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	s := NewStream(context.Background(), srv.Client(), srv.URL, map[string]any{}, map[string]string{}, maxRetries)
	s.sleep = func(time.Duration) {} // no backoff delay in tests
	return s, srv.Close
}

func drain(s *Stream) ([]translate.Event, error) {
	var got []translate.Event
	for {
		ev, err := s.Next()
		if err != nil {
			if err == io.EOF {
				return got, nil
			}
			return got, err
		}
		got = append(got, ev)
	}
}

func TestRetry5xxThenSucceeds(t *testing.T) {
	calls := 0
	s, closeSrv := newTestStream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(500)
			io.WriteString(w, `{"error":{"message":"oops"}}`)
			return
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"type":"finish","finishReason":"stop"}`+"\n")
	}, 2)
	defer closeSrv()

	got, err := drain(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2", calls)
	}
	if len(got) != 1 || got[0]["type"] != "finish" {
		t.Fatalf("events=%v", got)
	}
}

func TestRetry5xxExhausts(t *testing.T) {
	calls := 0
	s, closeSrv := newTestStream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"message":"down"}}`)
	}, 2)
	defer closeSrv()

	_, err := s.Next()
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Status != 503 {
		t.Fatalf("err=%v", err)
	}
	if calls != 3 { // initial + 2 retries
		t.Fatalf("calls=%d want 3", calls)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	calls := 0
	s, closeSrv := newTestStream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(403)
		io.WriteString(w, `{"error":{"code":"FORBIDDEN","message":"no"}}`)
	}, 2)
	defer closeSrv()

	_, err := s.Next()
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Status != 403 {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1 (no retry on 4xx)", calls)
	}
}

func TestInStreamRetryableErrorBeforeContent(t *testing.T) {
	calls := 0
	s, closeSrv := newTestStream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(200)
		if calls == 1 {
			io.WriteString(w, `{"type":"error","error":{"message":"transient","isRetryable":true}}`+"\n")
			return
		}
		io.WriteString(w, `{"type":"text-delta","text":"ok"}`+"\n")
		io.WriteString(w, `{"type":"finish","finishReason":"stop"}`+"\n")
	}, 2)
	defer closeSrv()

	got, err := drain(s)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2 (retried the isRetryable error)", calls)
	}
	if len(got) != 2 || got[0]["type"] != "text-delta" {
		t.Fatalf("events=%v", got)
	}
}
