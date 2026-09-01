// Package probe queries Command Code through the local proxy to determine
// which models a given account key may use (its plan), and caches the global
// model catalog from /provider/v1/models.
//
// The per-key probe sends a minimal non-streaming completion for each
// candidate model. The verdict comes from the proxy's HTTP status plus the
// upstream error body: Command Code signals account-level credit exhaustion
// with HTTP 400 "insufficient credits" (not 402!), and per-model plan gates
// with 402/403 MODEL_NOT_IN_PLAN. 200/429 count as usable.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Result is the outcome of probing one key against a set of models.
type Result struct {
	Allowed      []string          `json:"allowed"`
	Denied       []string          `json:"denied"`
	QuotaHeaders map[string]string `json:"quotaHeaders,omitempty"`
	// Blocked means the account itself cannot make requests (no credits /
	// billing gate); Reason carries the human-readable verdict.
	Blocked bool   `json:"blocked"`
	Reason  string `json:"reason,omitempty"`
}

type outcome int

const (
	outcomeAllowed outcome = iota
	outcomeDenied
	outcomeBlocked // account-level gate (no credits / billing)
	outcomeError
)

// classifyUpstreamError maps a proxy error body to a plan verdict.
func classifyUpstreamError(code, message string) (denied, blocked bool, reason string) {
	c := strings.ToLower(code + " " + message)
	switch {
	case strings.Contains(c, "model_not_in_plan"):
		return true, false, "模型不在套餐内"
	case strings.Contains(c, "insufficient credit"), strings.Contains(c, "purchase more"),
		strings.Contains(c, "no credits"), strings.Contains(c, "top up"), strings.Contains(c, "top-up"):
		return true, true, "账户额度不足（未购买套餐或已用尽）"
	case strings.Contains(c, "quota"), strings.Contains(c, "billing"),
		strings.Contains(c, "subscription"), strings.Contains(c, "upgrade_required"),
		strings.Contains(c, "plan"):
		return true, false, "套餐/账单限制"
	}
	return false, false, ""
}

// parseErrBody extracts error.code / error.message from the proxy's JSON
// error envelope.
func parseErrBody(raw []byte) (code, message string) {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		return e.Error.Code, e.Error.Message
	}
	return "", strings.TrimSpace(string(raw))
}

// Prober talks to the local proxy (OpenAI-compatible) with per-account keys.
type Prober struct {
	baseURL string
	client  *http.Client
}

// New creates a prober against the local proxy at baseURL.
func New(baseURL string) *Prober {
	return &Prober{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 45 * time.Second},
	}
}

// Plan probes each model with the account key. When a probe reveals the
// account itself is blocked (no credits), remaining probes are cancelled —
// the verdict is the same for every model.
func (p *Prober) Plan(ctx context.Context, apiKey string, models []string) (*Result, error) {
	if len(models) == 0 {
		return nil, fmt.Errorf("no probe models configured")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		res     = &Result{}
		errs    []string
		seenErr string
		sem     = make(chan struct{}, 3)
	)
	for _, model := range models {
		wg.Add(1)
		go func(model string) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return // account already judged blocked
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()
			o, quota, reason := p.probeModel(ctx, apiKey, model)
			mu.Lock()
			defer mu.Unlock()
			switch o {
			case outcomeAllowed:
				res.Allowed = append(res.Allowed, model)
				if res.QuotaHeaders == nil && len(quota) > 0 {
					res.QuotaHeaders = quota
				}
			case outcomeDenied:
				res.Denied = append(res.Denied, model)
				if reason != "" {
					res.Reason = reason
				}
			case outcomeBlocked:
				res.Blocked = true
				if reason != "" {
					res.Reason = reason
				}
				cancel() // stop other probes; account-level verdict
			default:
				if ctx.Err() != nil {
					return // cancelled mid-flight because the account is blocked
				}
				errs = append(errs, model)
				if seenErr == "" && reason != "" {
					seenErr = reason
				}
			}
		}(model)
	}
	wg.Wait()
	if res.Blocked {
		// Cancelled mid-flight models share the blocked verdict; fold them in.
		res.Denied = append(res.Denied, models...)
		return res, nil
	}
	if len(res.Allowed) == 0 && len(res.Denied) == 0 && len(errs) > 0 {
		if seenErr != "" {
			return nil, fmt.Errorf("探测失败：%s", seenErr)
		}
		return nil, fmt.Errorf("probe failed for all %d models — is the proxy running and the key valid?", len(errs))
	}
	return res, nil
}

func (p *Prober) probeModel(ctx context.Context, apiKey, model string) (outcome outcome, quota map[string]string, reason string) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return outcomeError, nil, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return outcomeError, nil, "无法连接本地代理：" + err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	quota = quotaHeaders(resp.Header)
	switch resp.StatusCode {
	case http.StatusOK:
		return outcomeAllowed, quota, ""
	case http.StatusTooManyRequests:
		return outcomeAllowed, quota, "" // throttled, but the model is usable
	case http.StatusForbidden, http.StatusPaymentRequired, http.StatusBadRequest, http.StatusUnauthorized:
		code, msg := parseErrBody(raw)
		denied, blocked, why := classifyUpstreamError(code, msg)
		switch {
		case blocked:
			return outcomeBlocked, quota, why
		case denied:
			return outcomeDenied, quota, why
		case resp.StatusCode == http.StatusUnauthorized:
			return outcomeError, quota, "密钥无效或已过期（401）"
		default:
			return outcomeError, quota, fmt.Sprintf("HTTP %d %s %s", resp.StatusCode, code, truncate(msg, 160))
		}
	default:
		code, msg := parseErrBody(raw)
		return outcomeError, quota, fmt.Sprintf("HTTP %d %s %s", resp.StatusCode, code, truncate(msg, 160))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var quotaPrefixes = []string{"x-ratelimit", "x-request", "retry-after", "anthropic-ratelimit"}

func quotaHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		for _, pre := range quotaPrefixes {
			if strings.HasPrefix(lk, pre) && len(v) > 0 {
				out[lk] = v[0]
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CatalogEntry is one model in Command Code's global catalog.
type CatalogEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
}

// Catalog fetched once per process (with TTL) from /provider/v1/models.
type Catalog struct {
	FetchedAt time.Time
	Models    []CatalogEntry
}

var (
	catalogMu    sync.Mutex
	catalogCache *Catalog
)

const catalogTTL = 24 * time.Hour

// FetchCatalog returns the global catalog. It does not require a key.
func FetchCatalog(ctx context.Context, modelsURL string) (*Catalog, error) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if catalogCache != nil && time.Since(catalogCache.FetchedAt) < catalogTTL {
		return catalogCache, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := catalogClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models endpoint: HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, err
	}
	c := &Catalog{FetchedAt: time.Now()}
	for _, m := range parsed.Data {
		c.Models = append(c.Models, CatalogEntry{ID: m.ID, Name: m.Name, ContextLength: m.ContextLength})
	}
	catalogCache = c
	return c, nil
}

var catalogClient = &http.Client{Timeout: 20 * time.Second}
