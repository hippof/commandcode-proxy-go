package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminPageServesHTML(t *testing.T) {
	srv := testServer("http://unused", nil)
	rec := do(srv, "GET", "/admin", "", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "commandcode-proxy") {
		t.Fatalf("missing title in dashboard HTML")
	}
}

func TestAdminDataLogsRequestWithModel(t *testing.T) {
	cc := httptest.NewServer(ccStream(200,
		`{"type":"text-delta","text":"Hi"}`, `{"type":"finish","finishReason":"stop"}`))
	defer cc.Close()
	srv := testServer(cc.URL, nil)

	do(srv, "POST", "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{authHdr: "Bearer user_x"})

	data := decode(t, do(srv, "GET", "/admin/data", "", nil))
	if data["total"].(float64) < 1 {
		t.Fatalf("total=%v", data["total"])
	}
	var entry map[string]any
	for _, ei := range data["recent"].([]any) {
		if e := ei.(map[string]any); e["path"] == "/v1/chat/completions" {
			entry = e
		}
	}
	if entry == nil {
		t.Fatalf("chat request not logged: %v", data["recent"])
	}
	if entry["method"] != "POST" || entry["status"].(float64) != 200 || entry["model"] != "m" {
		t.Fatalf("entry=%v", entry)
	}
}

func TestAdminAndHealthExcludedFromLog(t *testing.T) {
	srv := testServer("http://unused", nil)
	do(srv, "GET", "/health", "", nil)
	do(srv, "GET", "/admin", "", nil)
	for _, ei := range decode(t, do(srv, "GET", "/admin/data", "", nil))["recent"].([]any) {
		p := ei.(map[string]any)["path"].(string)
		if p == "/health" || strings.HasPrefix(p, "/admin") {
			t.Fatalf("excluded path was logged: %s", p)
		}
	}
}
