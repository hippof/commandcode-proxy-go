package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxLogEntries = 200

// logEntry is request metadata only — never the API key or message content.
type logEntry struct {
	TS     float64 `json:"ts"`
	Method string  `json:"method"`
	Path   string  `json:"path"`
	Model  any     `json:"model"` // string, or null when the route set none
	Status int     `json:"status"`
	MS     int64   `json:"ms"`
}

// requestLog is a bounded in-memory ring of request metadata for the dashboard.
// It is cleared on restart and never affects request handling, so the proxy
// stays semantically stateless.
type requestLog struct {
	mu      sync.Mutex
	entries []logEntry
	total   int
	started time.Time
}

func newRequestLog() *requestLog { return &requestLog{started: time.Now()} }

func (rl *requestLog) add(e logEntry) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.total++
	rl.entries = append(rl.entries, e)
	if len(rl.entries) > maxLogEntries {
		rl.entries = append(rl.entries[:0], rl.entries[len(rl.entries)-maxLogEntries:]...)
	}
}

func (rl *requestLog) snapshot() map[string]any {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	recent := make([]logEntry, len(rl.entries))
	for i, e := range rl.entries { // newest first
		recent[len(rl.entries)-1-i] = e
	}
	return map[string]any{
		"uptime_seconds": int(time.Since(rl.started).Seconds()),
		"total":          rl.total,
		"recent":         recent,
	}
}

// modelCarrier flows the resolved model from a route handler back to the logging
// middleware (the Go analog of the ASGI scope state).
type modelCarrier struct {
	model string
	set   bool
}

type ctxKey int

const modelKey ctxKey = 0

func setModel(r *http.Request, model string) {
	if mc, ok := r.Context().Value(modelKey).(*modelCarrier); ok {
		mc.model = model
		mc.set = true
	}
}

// withLogging records method/path/model/status/latency per request, skipping the
// dashboard's own polling (/admin*) and health checks. It wraps the
// ResponseWriter but delegates http.Flusher so SSE streaming is unaffected.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/health" || strings.HasPrefix(path, "/admin") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		mc := &modelCarrier{}
		r = r.WithContext(context.WithValue(r.Context(), modelKey, mc))
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, r)

		var model any
		if mc.set {
			model = mc.model
		}
		ms := time.Since(start).Milliseconds()
		s.log.add(logEntry{
			TS:     float64(time.Now().UnixNano()) / 1e9,
			Method: r.Method,
			Path:   path,
			Model:  model,
			Status: sw.status,
			MS:     ms,
		})
		if s.cfg.LogEnabled("info") {
			if mc.set {
				log.Printf("%s %s %d %dms model=%s", r.Method, path, sw.status, ms, mc.model)
			} else {
				log.Printf("%s %s %d %dms", r.Method, path, sw.status, ms)
			}
		}
	})
}

// statusWriter captures the response status while passing flushing through, so
// streamed (SSE) responses are not buffered.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) adminData(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.log.snapshot())
}

func (s *Server) adminPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(adminHTML))
}

const adminHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>commandcode-proxy</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 14px/1.5 system-ui, -apple-system, sans-serif; margin: 0; padding: 1.5rem; }
  h1 { font-size: 1.05rem; margin: 0 0 1rem; display: flex; align-items: center; gap: .5rem; }
  .dot { width: .6rem; height: .6rem; border-radius: 50%; background: #16a34a; }
  .stats { display: flex; gap: 2rem; margin-bottom: 1rem; color: #6b7280; }
  .stats b { color: inherit; font-variant-numeric: tabular-nums; }
  table { border-collapse: collapse; width: 100%; font-variant-numeric: tabular-nums; }
  th, td { text-align: left; padding: .35rem .6rem; border-bottom: 1px solid rgba(128,128,128,.2); white-space: nowrap; }
  th { color: #6b7280; font-weight: 600; }
  td.path { white-space: normal; word-break: break-all; }
  .ok { color: #16a34a; } .err { color: #dc2626; } .muted { color: #9ca3af; }
</style>
</head>
<body>
  <h1><span class="dot"></span>commandcode-proxy</h1>
  <div class="stats">
    <div>uptime <b id="uptime">&ndash;</b></div>
    <div>requests <b id="total">&ndash;</b></div>
    <div class="muted" id="updated"></div>
  </div>
  <table>
    <thead><tr><th>time</th><th>method</th><th>path</th><th>model</th><th>status</th><th>ms</th></tr></thead>
    <tbody id="rows"></tbody>
  </table>
<script>
function fmtUptime(s){ s=Math.floor(s); const d=Math.floor(s/86400),h=Math.floor(s%86400/3600),m=Math.floor(s%3600/60);
  if(d)return d+'d '+h+'h'; if(h)return h+'h '+m+'m'; if(m)return m+'m '+(s%60)+'s'; return s+'s'; }
function esc(x){ return (x==null?'':String(x)).replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c])); }
async function tick(){
  try{
    const d = await (await fetch('/admin/data')).json();
    document.getElementById('uptime').textContent = fmtUptime(d.uptime_seconds);
    document.getElementById('total').textContent = d.total;
    document.getElementById('updated').textContent = 'updated ' + new Date().toLocaleTimeString();
    document.getElementById('rows').innerHTML = (d.recent||[]).map(function(e){
      const cls = e.status>=200 && e.status<400 ? 'ok' : 'err';
      const t = new Date(e.ts*1000).toLocaleTimeString();
      return '<tr><td class="muted">'+esc(t)+'</td><td>'+esc(e.method)+'</td><td class="path">'+esc(e.path)+
             '</td><td>'+esc(e.model)+'</td><td class="'+cls+'">'+esc(e.status)+'</td><td>'+esc(e.ms)+'</td></tr>';
    }).join('');
  }catch(_){ document.getElementById('updated').textContent = 'disconnected'; }
}
tick(); setInterval(tick, 3000);
</script>
</body>
</html>`
