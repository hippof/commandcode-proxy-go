// Package gateway is a loopback HTTP gateway that sits in front of the local
// commandcode-proxy and replaces whatever credential a client sent with the
// account currently active in the tray.
//
// Why it exists: the proxy is keyless — it relays the caller's key verbatim —
// and most editors/IDEs can only store a static key in their provider config
// (ZCode, Cursor, Cline, Continue, …). Pointing those clients at this gateway
// once means a tray switch takes effect on their next request, without editing
// any client configuration.
//
// The gateway is transparent otherwise: every path, query and header (except
// the credential) is forwarded to the proxy unchanged, and streaming responses
// are flushed straight through.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"commandcode-desktop/internal/applog"
)

// credentialHeaders are dropped from the incoming request: the gateway decides
// the credential, the client's value is only a placeholder.
var credentialHeaders = []string{"Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key"}

// KeyFunc resolves the credential to use for a request. An error means "no
// account is active" and is reported to the client as a 401.
type KeyFunc func() (string, error)

// Status is the UI-facing state of the gateway.
type Status struct {
	Running   bool   `json:"running"`
	Addr      string `json:"addr"` // e.g. http://127.0.0.1:8790
	LastError string `json:"lastError,omitempty"`
}

// UpstreamFunc reports where the proxy currently listens. It is resolved per
// request because the proxy falls back to another port when its configured one
// is taken — the gateway must never forward to a stale address.
type UpstreamFunc func() string

// Gateway is a local credential-injecting reverse proxy.
type Gateway struct {
	upstreamFn UpstreamFunc
	keyFn      KeyFunc
	ensure     func() error // optional: bring the upstream up before forwarding
	proxy      *httputil.ReverseProxy

	// noAccountLogged throttles the "no active account" warning so an editor
	// retrying in a loop cannot flood the log.
	noAccountLogged time.Time

	mu      sync.Mutex
	srv     *http.Server
	ln      net.Listener
	addr    string
	lastErr string
}

// keyCtx/targetCtx carry per-request resolution results into the rewrite step.
type keyCtx struct{}
type targetCtx struct{}

// New builds a gateway that forwards to whatever upstream() reports at request
// time (e.g. "http://127.0.0.1:8787"). ensure, when set, is called first so a
// stopped proxy can be started on demand instead of surfacing a connection
// error in the editor.
func New(upstream UpstreamFunc, keyFn KeyFunc, ensure func() error) (*Gateway, error) {
	if upstream == nil {
		return nil, errors.New("gateway: no upstream resolver")
	}
	g := &Gateway{upstreamFn: upstream, keyFn: keyFn, ensure: ensure}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, _ := pr.In.Context().Value(targetCtx{}).(*url.URL)
			if target == nil {
				return
			}
			pr.SetURL(target)
			pr.Out.Host = target.Host
			for _, h := range credentialHeaders {
				pr.Out.Header.Del(h)
			}
			if key, ok := pr.In.Context().Value(keyCtx{}).(string); ok && key != "" {
				// Both surfaces are covered: the proxy prefers x-api-key for
				// /v1/messages and reads Authorization for the OpenAI routes.
				pr.Out.Header.Set("Authorization", "Bearer "+key)
				pr.Out.Header.Set("X-Api-Key", key)
			}
		},
		// Never buffer: SSE from the proxy must reach the client as it arrives.
		FlushInterval: -1,
		Transport: &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			ForceAttemptHTTP2: false,
			MaxIdleConns:      16,
			IdleConnTimeout:   60 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			applog.Error("gateway", err, "path", r.URL.Path, "stage", "upstream")
			writeError(w, r, http.StatusBadGateway, "upstream unreachable: "+err.Error())
		},
	}
	return g, nil
}

// Addr is the gateway's base URL ("" while stopped).
func (g *Gateway) Addr() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.addr
}

// Upstream is the proxy address the gateway would use right now ("" when the
// gateway is stopped). Exposed so the tray can show which port it targets.
func (g *Gateway) Upstream() string {
	if g.upstreamFn == nil {
		return ""
	}
	return g.upstreamFn()
}

// Status reports whether the gateway is listening.
func (g *Gateway) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Status{Running: g.ln != nil, Addr: g.addr, LastError: g.lastErr}
}

// Start begins listening on host:port. Starting twice is a no-op.
func (g *Gateway) Start(host string, port int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ln != nil {
		return nil
	}
	if host == "" {
		host = "127.0.0.1"
	}
	// port 0 asks the OS for a free port (used by tests; the app always passes
	// the configured port explicitly).
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		g.lastErr = err.Error()
		return fmt.Errorf("网关无法监听 %s（端口被占用？）：%w", addr, err)
	}
	srv := &http.Server{
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
	}
	g.ln, g.srv = ln, srv
	g.addr = "http://" + ln.Addr().String()
	g.lastErr = ""
	applog.Info("gateway", "开始监听", "addr", g.addr, "upstream", g.upstreamFn())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			applog.Error("gateway", err, "stage", "serve")
			g.mu.Lock()
			g.lastErr = err.Error()
			g.mu.Unlock()
		}
	}()
	return nil
}

// Stop closes the listener (idempotent).
func (g *Gateway) Stop() error {
	g.mu.Lock()
	srv, ln := g.srv, g.ln
	g.srv, g.ln = nil, nil
	g.mu.Unlock()
	if srv == nil || ln == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// ServeHTTP resolves the active credential and forwards the request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, err := g.keyFn()
	if err != nil || key == "" {
		msg := "没有激活的账号：请在托盘里「切换」一个账号，或用 cmdc login 登录后「保存当前登录凭证」"
		if err != nil {
			msg = err.Error()
		}
		if g.shouldLogNoAccount(time.Now()) {
			applog.Warn("gateway", "没有激活账号，已拒绝请求", "path", r.URL.Path, "detail", msg)
		}
		writeError(w, r, http.StatusUnauthorized, msg)
		return
	}
	if g.ensure != nil {
		if err := g.ensure(); err != nil {
			applog.Error("gateway", err, "stage", "start-upstream", "path", r.URL.Path)
			writeError(w, r, http.StatusBadGateway, "本地代理无法启动："+err.Error())
			return
		}
	}
	// Resolve the upstream *after* ensure: starting the proxy may have moved it
	// to a fallback port.
	target, err := url.Parse(strings.TrimRight(strings.TrimSpace(g.upstreamFn()), "/"))
	if err != nil || target.Host == "" {
		err := fmt.Errorf("代理地址不可用：%v (%q)", err, g.upstreamFn())
		applog.Error("gateway", err, "stage", "resolve-upstream", "path", r.URL.Path)
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	ctx := context.WithValue(r.Context(), keyCtx{}, key)
	ctx = context.WithValue(ctx, targetCtx{}, target)
	g.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// shouldLogNoAccount rate-limits the no-account warning to once a minute.
func (g *Gateway) shouldLogNoAccount(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Sub(g.noAccountLogged) < time.Minute {
		return false
	}
	g.noAccountLogged = now
	return true
}

// writeError answers in the shape the client expects for that surface, so the
// message shows up in the editor's UI instead of a bare status code.
func writeError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var body any
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		body = map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "authentication_error", "message": msg},
		}
	} else {
		body = map[string]any{
			"error": map[string]any{
				"message": msg,
				"type":    "invalid_request_error",
				"code":    "no_active_account",
			},
		}
	}
	_ = json.NewEncoder(w).Encode(body)
}
