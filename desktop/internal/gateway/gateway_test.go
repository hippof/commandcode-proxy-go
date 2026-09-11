package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// upstreamSpy is a fake commandcode-proxy that records the headers it saw.
type upstreamSpy struct {
	auth  chan string
	xkey  chan string
	path  chan string
	query chan string
	calls int32
}

func newUpstreamSpy() (*httptest.Server, *upstreamSpy) {
	spy := &upstreamSpy{
		auth:  make(chan string, 8),
		xkey:  make(chan string, 8),
		path:  make(chan string, 8),
		query: make(chan string, 8),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&spy.calls, 1)
		spy.auth <- r.Header.Get("Authorization")
		spy.xkey <- r.Header.Get("X-Api-Key")
		spy.path <- r.URL.Path
		spy.query <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	return srv, spy
}

// startGateway boots a gateway against upstream with a mutable key source.
func startGateway(t *testing.T, upstream string) (*Gateway, *atomic.Value) {
	t.Helper()
	key := &atomic.Value{}
	key.Store("user_active_key_0001")
	g, err := New(func() string { return upstream }, func() (string, error) {
		v, _ := key.Load().(string)
		if v == "" {
			return "", errNoAccount
		}
		return v, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start("127.0.0.1", 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Stop() })
	return g, key
}

var errNoAccount = &noAccountError{}

type noAccountError struct{}

func (*noAccountError) Error() string { return "没有激活的账号（测试）" }

func TestGatewayReplacesClientCredential(t *testing.T) {
	up, spy := newUpstreamSpy()
	defer up.Close()
	g, _ := startGateway(t, up.URL)

	cases := []struct {
		name   string
		header map[string]string
	}{
		{"no credential", nil},
		{"placeholder bearer", map[string]string{"Authorization": "Bearer sk-local-placeholder"}},
		{"placeholder x-api-key", map[string]string{"X-Api-Key": "sk-whatever"}},
		{"both wrong", map[string]string{"Authorization": "Bearer bogus", "X-Api-Key": "bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, g.Addr()+"/v1/messages", strings.NewReader(`{"model":"x"}`))
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)

			if got := <-spy.auth; got != "Bearer user_active_key_0001" {
				t.Fatalf("upstream Authorization = %q", got)
			}
			if got := <-spy.xkey; got != "user_active_key_0001" {
				t.Fatalf("upstream X-Api-Key = %q", got)
			}
		})
	}
}

// TestGatewayFollowsKeySourcePerRequest is the whole point: after a tray
// switch, the next request from a static-key client must use the new account.
func TestGatewayFollowsKeySourcePerRequest(t *testing.T) {
	up, spy := newUpstreamSpy()
	defer up.Close()
	g, key := startGateway(t, up.URL)

	for _, want := range []string{"user_active_key_0001", "user_second_account_2", "user_active_key_0001"} {
		key.Store(want)
		resp, err := http.Post(g.Addr()+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got := <-spy.auth; got != "Bearer "+want {
			t.Fatalf("upstream saw %q, want the current key %q", got, want)
		}
	}
}

func TestGatewayNoAccountIsReportedPerSurface(t *testing.T) {
	up, spy := newUpstreamSpy()
	defer up.Close()
	g, key := startGateway(t, up.URL)
	key.Store("")

	t.Run("anthropic surface", func(t *testing.T) {
		resp, err := http.Post(g.Addr()+"/v1/messages", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Type != "error" || body.Error.Type != "authentication_error" || !strings.Contains(body.Error.Message, "没有激活") {
			t.Fatalf("anthropic error body = %+v", body)
		}
	})

	t.Run("openai surface", func(t *testing.T) {
		resp, err := http.Post(g.Addr()+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		var body struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != "no_active_account" || !strings.Contains(body.Error.Message, "没有激活") {
			t.Fatalf("openai error body = %+v", body)
		}
	})

	if n := atomic.LoadInt32(&spy.calls); n != 0 {
		t.Fatalf("upstream must not be called without an account, got %d calls", n)
	}
}

func TestGatewayForwardsPathQueryAndStreamsUnbuffered(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" || r.URL.RawQuery != "beta=1" {
			t.Errorf("upstream path/query = %q?%q", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		io.WriteString(w, "data: first\n\n")
		fl.Flush()
		<-release // hold the stream open: a buffering gateway would stall here
		io.WriteString(w, "data: second\n\n")
		fl.Flush()
	}))
	defer up.Close()

	g, _ := startGateway(t, up.URL)
	resp, err := http.Get(g.Addr() + "/v1/messages/count_tokens?beta=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				lines <- line
			}
		}
		close(lines)
	}()

	select {
	case got := <-lines:
		if got != "data: first" {
			t.Fatalf("first chunk = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first SSE chunk did not arrive while the upstream was still streaming (gateway buffered it)")
	}
	close(release)
	select {
	case got := <-lines:
		if got != "data: second" {
			t.Fatalf("second chunk = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second chunk missing")
	}
}

func TestGatewayLifecycle(t *testing.T) {
	up, _ := newUpstreamSpy()
	defer up.Close()

	g, err := New(func() string { return up.URL }, func() (string, error) { return "k", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st := g.Status(); st.Running || st.Addr != "" {
		t.Fatalf("status before start = %+v", st)
	}
	if err := g.Start("127.0.0.1", 0); err != nil {
		t.Fatal(err)
	}
	if st := g.Status(); !st.Running || !strings.HasPrefix(st.Addr, "http://127.0.0.1:") {
		t.Fatalf("status after start = %+v", st)
	}
	// Starting again is a no-op, not an error.
	if err := g.Start("127.0.0.1", 0); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if err := g.Stop(); err != nil {
		t.Fatal(err)
	}
	if st := g.Status(); st.Running {
		t.Fatal("status still running after stop")
	}
	if err := g.Stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

func TestGatewayPortInUseIsAClearError(t *testing.T) {
	up, _ := newUpstreamSpy()
	defer up.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	g, err := New(func() string { return up.URL }, func() (string, error) { return "k", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	portNum, _ := strconv.Atoi(port)
	err = g.Start("127.0.0.1", portNum)
	if err == nil {
		t.Fatal("want an error when the port is taken")
	}
	if !strings.Contains(err.Error(), "无法监听") {
		t.Fatalf("error should explain the port problem, got: %v", err)
	}
	if st := g.Status(); st.Running || st.LastError == "" {
		t.Fatalf("status = %+v, want stopped with a recorded error", st)
	}
}

// TestGatewayFollowsUpstreamAddressChange is the "never forward to a stale
// port" guarantee: the proxy may fall back to another port when its configured
// one is taken, and the gateway resolves the address per request.
func TestGatewayFollowsUpstreamAddressChange(t *testing.T) {
	upA, spyA := newUpstreamSpy()
	defer upA.Close()
	upB, spyB := newUpstreamSpy()
	defer upB.Close()

	current := &atomic.Value{}
	current.Store(upA.URL)
	g, err := New(func() string { return current.Load().(string) },
		func() (string, error) { return "user_key_x", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start("127.0.0.1", 0); err != nil {
		t.Fatal(err)
	}
	defer g.Stop()

	post := func() {
		t.Helper()
		resp, err := http.Post(g.Addr()+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	post()
	if n := atomic.LoadInt32(&spyA.calls); n != 1 {
		t.Fatalf("first request should reach upstream A, calls = %d", n)
	}

	// The proxy moved ports; the very next request must follow it.
	current.Store(upB.URL)
	post()
	if n := atomic.LoadInt32(&spyB.calls); n != 1 {
		t.Fatalf("request after the move should reach upstream B, calls = %d", n)
	}
	if n := atomic.LoadInt32(&spyA.calls); n != 1 {
		t.Fatalf("upstream A must not be used again, calls = %d", n)
	}
}

// TestGatewayReportsUnusableUpstream covers a bad/empty resolver.
func TestGatewayReportsUnusableUpstream(t *testing.T) {
	g, err := New(func() string { return "://not-a-url" },
		func() (string, error) { return "user_key_x", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start("127.0.0.1", 0); err != nil {
		t.Fatal(err)
	}
	defer g.Stop()

	resp, err := http.Post(g.Addr()+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}
