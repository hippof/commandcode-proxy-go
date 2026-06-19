// Package server wires the HTTP routes to the translation pipeline.
//
// Public contracts: OpenAI Chat Completions (/v1/chat/completions, /v1/models)
// and Anthropic Messages (/v1/messages), plus /health. Every request peeks the
// first decisive upstream event so plan/non-200 errors become a real HTTP status
// before any bytes stream to the client.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/liwei/commandcode-proxy-go/internal/auth"
	"github.com/liwei/commandcode-proxy-go/internal/config"
	"github.com/liwei/commandcode-proxy-go/internal/translate"
	"github.com/liwei/commandcode-proxy-go/internal/upstream"
)

// Server holds the resolved config and a shared HTTP client.
type Server struct {
	cfg    *config.Config
	client *http.Client
	log    *requestLog
}

// New builds a Server with a streaming-friendly HTTP client (bounded connect and
// response-header waits, but no total timeout so long streams aren't cut).
func New(cfg *config.Config) *Server {
	return &Server{
		cfg: cfg,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
				ResponseHeaderTimeout: cfg.RequestTimeout,
				ForceAttemptHTTP2:     true,
			},
		},
		log: newRequestLog(),
	}
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("POST /v1/messages", s.messages)
	mux.HandleFunc("GET /admin/data", s.adminData)
	mux.HandleFunc("GET /admin", s.adminPage)
	return s.withLogging(mux)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	key := auth.ResolveAPIKey(r.Header.Get("Authorization"), "")
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, s.cfg.ModelsURL, nil)
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "failed to fetch models: "+err.Error(), "upstream_error", "")
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		writeOpenAIError(w, resp.StatusCode, "failed to fetch models: "+string(data), "upstream_error", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, errMsg := readJSONObject(r)
	if errMsg != "" {
		writeOpenAIError(w, http.StatusBadRequest, errMsg, "invalid_request_error", "")
		return
	}
	key := auth.ResolveAPIKey(r.Header.Get("Authorization"), "")
	if key == "" {
		writeOpenAIError(w, http.StatusUnauthorized,
			"Missing Command Code API key. Send it as 'Authorization: Bearer <key>'.",
			"authentication_error", "")
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "'model' is required", "invalid_request_error", "")
		return
	}
	model = s.cfg.ResolveModel(model)
	body["model"] = model
	setModel(r, model)

	stream, _ := body["stream"].(bool)
	includeUsage := false
	if so, ok := body["stream_options"].(map[string]any); ok {
		includeUsage, _ = so["include_usage"].(bool)
	}
	cid := "chatcmpl-" + randHex(16)
	created := translate.NowUnix()

	ccBody := translate.BuildCCRequest(body, s.cfg)
	src := upstream.NewStream(r.Context(), s.client, s.cfg.GenerateURL, ccBody, s.ccHeaders(key), s.cfg.MaxRetries)
	defer src.Close()

	first, err := upstream.AdvanceToDecisive(src)
	if err != nil {
		st, msg, code := upstreamStatus(err)
		writeOpenAIError(w, st, msg, "upstream_error", code)
		return
	}
	if first == nil {
		if stream {
			s.streamSSE(w, func(emit func(string) error) error {
				return translate.EmptyStream(cid, created, model, emit)
			})
		} else {
			writeJSON(w, http.StatusOK, translate.EmptyCompletion(cid, created, model))
		}
		return
	}
	if t, _ := first["type"].(string); t == "error" {
		e := errorObject(first)
		writeOpenAIError(w, statusFromCCError(e), errMessage(e), "upstream_error", errCode(e))
		return
	}

	next := streamNext(src)
	if stream {
		s.streamSSE(w, func(emit func(string) error) error {
			return translate.StreamCompletion(first, next, model, cid, created, includeUsage, emit)
		})
		return
	}
	completion, ccErr := translate.BuildCompletion(first, next, model, cid, created)
	if ccErr != nil {
		writeOpenAIError(w, statusFromCCError(ccErr), errMessage(ccErr), "upstream_error", errCode(ccErr))
		return
	}
	writeJSON(w, http.StatusOK, completion)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	body, errMsg := readJSONObject(r)
	if errMsg != "" {
		writeAnthropicError(w, http.StatusBadRequest, errMsg)
		return
	}
	key := auth.ResolveAPIKey(r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
	if key == "" {
		writeAnthropicError(w, http.StatusUnauthorized,
			"Missing Command Code API key. Send it as 'x-api-key: <key>' or 'Authorization: Bearer <key>'.")
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "'model' is required")
		return
	}
	if body["max_tokens"] == nil {
		writeAnthropicError(w, http.StatusBadRequest, "'max_tokens' is required")
		return
	}
	model = s.cfg.ResolveModel(model)
	setModel(r, model)

	stream, _ := body["stream"].(bool)
	mid := "msg_" + randHex(16)

	openaiReq := translate.OpenAIRequestFromAnthropic(body)
	openaiReq["model"] = model
	ccBody := translate.BuildCCRequest(openaiReq, s.cfg)
	src := upstream.NewStream(r.Context(), s.client, s.cfg.GenerateURL, ccBody, s.ccHeaders(key), s.cfg.MaxRetries)
	defer src.Close()

	first, err := upstream.AdvanceToDecisive(src)
	if err != nil {
		st, msg, _ := upstreamStatus(err)
		writeAnthropicError(w, st, msg)
		return
	}
	if first == nil {
		if stream {
			s.streamSSE(w, func(emit func(string) error) error {
				return translate.EmptyMessageStream(mid, model, emit)
			})
		} else {
			writeJSON(w, http.StatusOK, translate.EmptyMessage(mid, model))
		}
		return
	}
	if t, _ := first["type"].(string); t == "error" {
		e := errorObject(first)
		writeAnthropicError(w, statusFromCCError(e), errMessage(e))
		return
	}

	next := streamNext(src)
	if stream {
		s.streamSSE(w, func(emit func(string) error) error {
			return translate.StreamMessage(first, next, model, mid, emit)
		})
		return
	}
	message, ccErr := translate.BuildMessage(first, next, model, mid)
	if ccErr != nil {
		writeAnthropicError(w, statusFromCCError(ccErr), errMessage(ccErr))
		return
	}
	writeJSON(w, http.StatusOK, message)
}

func (s *Server) ccHeaders(key string) map[string]string {
	return map[string]string{
		"Authorization":          "Bearer " + key,
		"Content-Type":           "application/json",
		"Accept":                 "text/event-stream",
		"x-command-code-version": s.cfg.CLIVersion,
		"x-cli-environment":      "production",
		"x-project-slug":         "commandcode-proxy",
		"x-taste-learning":       s.cfg.TasteLearning,
		"x-co-flag":              "false",
	}
}

// streamSSE writes an event-stream response, flushing after each frame.
func (s *Server) streamSSE(w http.ResponseWriter, gen func(emit func(string) error) error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	emit := func(frame string) error {
		if _, err := io.WriteString(w, frame); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	_ = gen(emit)
}

func streamNext(s *upstream.Stream) translate.NextFunc {
	return func() (translate.Event, bool) {
		ev, err := s.Next()
		if err != nil {
			return nil, false
		}
		return ev, true
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func upstreamStatus(err error) (int, string, string) {
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) {
		return ue.Status, ue.Message, ue.Code
	}
	return http.StatusBadGateway, err.Error(), ""
}

func errorObject(ev map[string]any) map[string]any {
	if e, ok := ev["error"].(map[string]any); ok {
		return e
	}
	return map[string]any{}
}

func errMessage(m map[string]any) string {
	if s, ok := m["message"].(string); ok && s != "" {
		return s
	}
	return "upstream error"
}

func errCode(m map[string]any) string {
	s, _ := m["code"].(string)
	return s
}

// statusFromCCError mirrors _status_from_cc_error: statusCode (truthy) then
// status, range-checked, else 502.
func statusFromCCError(err map[string]any) int {
	n := asInt(err["statusCode"])
	if n == 0 {
		n = asInt(err["status"])
	}
	if n >= 400 && n < 600 {
		return n
	}
	return http.StatusBadGateway
}

func asInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

var anthropicErrorTypes = map[int]string{
	400: "invalid_request_error",
	401: "authentication_error",
	403: "permission_error",
	404: "not_found_error",
	413: "request_too_large",
	429: "rate_limit_error",
	529: "overloaded_error",
}

func writeOpenAIError(w http.ResponseWriter, status int, message, etype, code string) {
	var codeVal any
	if code != "" {
		codeVal = code
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": etype, "code": codeVal},
	})
}

func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	etype := anthropicErrorTypes[status]
	if etype == "" {
		etype = "api_error"
	}
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": etype, "message": message},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// readJSONObject reads the request body as a JSON object, returning a non-empty
// error message string on failure (so the caller can format an API-specific
// error).
func readJSONObject(r *http.Request) (map[string]any, string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, "request body must be valid JSON"
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil, "request body must be valid JSON"
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, "request body must be a JSON object"
	}
	return m, ""
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
