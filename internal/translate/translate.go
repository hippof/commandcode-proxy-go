// Package translate converts between the OpenAI Chat Completions / Anthropic
// Messages shapes and Command Code's /alpha/generate protocol.
//
// It is pure: no network, file, or global mutable state — all I/O lives in the
// upstream and server packages. The Command Code wire format mirrors the upstream
// pi extension: messages become typed parts (text / reasoning / tool-call /
// tool-result), tools carry an input_schema, and the response is a
// newline-delimited Vercel-AI-SDK event stream (text-delta / reasoning-delta /
// tool-call / finish / error).
package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/liwei/commandcode-proxy-go/internal/config"
)

// Event is one upstream Command Code stream event (a parsed JSON object).
type Event = map[string]any

// NextFunc pulls the next event from a source, returning ok=false when drained.
type NextFunc func() (Event, bool)

// ── Request: OpenAI -> Command Code ──────────────────────────────────────────

// BuildCCRequest builds the Command Code /alpha/generate body from an OpenAI
// request. The caller has already resolved req["model"] to a Command Code id.
func BuildCCRequest(req map[string]any, cfg *config.Config) map[string]any {
	ccMessages, systemText := splitSystemAndMessages(getList(req["messages"]))

	var temperature any = req["temperature"]
	if temperature == nil {
		temperature = cfg.DefaultTemperature
	}

	return map[string]any{
		"config": configBlock(cfg),
		"memory": nil,
		"taste":  nil,
		"skills": nil,
		"params": map[string]any{
			"model":       req["model"],
			"messages":    ccMessages,
			"tools":       toolsToCC(getList(req["tools"])),
			"system":      systemText,
			"max_tokens":  resolveMaxTokens(req, cfg),
			"temperature": temperature,
			"stream":      true,
		},
		"threadId": newUUID(),
	}
}

// configBlock is a neutral context block. Command Code uses it for grounding;
// minimal values are accepted.
func configBlock(cfg *config.Config) map[string]any {
	return map[string]any{
		"workingDir":    cfg.WorkingDir,
		"date":          time.Now().Format("2006-01-02"),
		"environment":   fmt.Sprintf("%s-%s, Go %s", runtime.GOOS, runtime.GOARCH, runtime.Version()),
		"structure":     []any{},
		"isGitRepo":     false,
		"currentBranch": "",
		"mainBranch":    "",
		"gitStatus":     "",
		"recentCommits": []any{},
	}
}

func resolveMaxTokens(req map[string]any, cfg *config.Config) int {
	chosen, has := firstTruthy(req["max_tokens"], req["max_completion_tokens"])
	if !has {
		return cfg.DefaultMaxTokens
	}
	n, ok := toInt(chosen)
	if !ok {
		return cfg.DefaultMaxTokens
	}
	if n > cfg.MaxTokensCap {
		return cfg.MaxTokensCap
	}
	return n
}

func splitSystemAndMessages(messages []any) ([]any, string) {
	var systemParts []string
	var convo []any
	for _, mi := range messages {
		m := getMap(mi)
		if m == nil {
			continue
		}
		if role := getStr(m, "role"); role == "system" || role == "developer" {
			if text := contentToText(m["content"]); text != "" {
				systemParts = append(systemParts, text)
			}
		} else {
			convo = append(convo, m)
		}
	}
	return messagesToCC(convo), strings.Join(systemParts, "\n\n")
}

func messagesToCC(messages []any) []any {
	paired := pairedToolCallIDs(messages)
	nameByID := toolNamesByID(messages)
	out := []any{}
	for _, mi := range messages {
		m := getMap(mi)
		if m == nil {
			continue
		}
		switch getStr(m, "role") {
		case "user":
			out = append(out, map[string]any{"role": "user", "content": userContentToCC(m["content"])})
		case "assistant":
			parts := []any{}
			if text := contentToText(m["content"]); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, tci := range getList(m["tool_calls"]) {
				tc := getMap(tci)
				if tc == nil {
					continue
				}
				cid := getStr(tc, "id")
				if cid == "" || !paired[cid] {
					continue
				}
				fn := getMap(tc["function"])
				parts = append(parts, map[string]any{
					"type":       "tool-call",
					"toolCallId": cid,
					"toolName":   getStr(fn, "name"),
					"input":      parseArguments(fn["arguments"]),
				})
			}
			if len(parts) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": parts})
			}
		case "tool":
			cid := getStr(m, "tool_call_id")
			if cid == "" || !paired[cid] {
				continue
			}
			name, ok := nameByID[cid]
			if !ok {
				name = getStr(m, "name")
			}
			out = append(out, map[string]any{
				"role": "tool",
				"content": []any{map[string]any{
					"type":       "tool-result",
					"toolCallId": cid,
					"toolName":   name,
					"output":     map[string]any{"type": "text", "value": contentToText(m["content"])},
				}},
			})
			// Command Code silently drops media parts inside tool results (probe
			// in docs/ROADMAP.md); the current CLI re-emits tool-result images as
			// a follow-up user turn instead, so mirror that.
			if imgs := imagePartsToCC(m["content"]); len(imgs) > 0 {
				out = append(out, map[string]any{"role": "user", "content": imgs})
			}
		}
	}
	return out
}

// pairedToolCallIDs returns ids present in BOTH an assistant tool_calls and a
// tool result. Command Code rejects dangling tool calls/results.
func pairedToolCallIDs(messages []any) map[string]bool {
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	for _, mi := range messages {
		m := getMap(mi)
		if m == nil {
			continue
		}
		switch getStr(m, "role") {
		case "assistant":
			for _, tci := range getList(m["tool_calls"]) {
				if tc := getMap(tci); tc != nil {
					if id := getStr(tc, "id"); id != "" {
						callIDs[id] = true
					}
				}
			}
		case "tool":
			if id := getStr(m, "tool_call_id"); id != "" {
				resultIDs[id] = true
			}
		}
	}
	paired := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			paired[id] = true
		}
	}
	return paired
}

func toolNamesByID(messages []any) map[string]string {
	names := map[string]string{}
	for _, mi := range messages {
		m := getMap(mi)
		if m == nil || getStr(m, "role") != "assistant" {
			continue
		}
		for _, tci := range getList(m["tool_calls"]) {
			if tc := getMap(tci); tc != nil {
				if id := getStr(tc, "id"); id != "" {
					names[id] = getStr(getMap(tc["function"]), "name")
				}
			}
		}
	}
	return names
}

func toolsToCC(tools []any) []any {
	out := []any{}
	for _, ti := range tools {
		t := getMap(ti)
		if t == nil || getStr(t, "type") != "function" {
			continue
		}
		fn := getMap(t["function"])
		params := getMap(fn["parameters"])
		if params == nil {
			params = map[string]any{}
		}
		out = append(out, map[string]any{
			"type":         "function",
			"name":         fn["name"],
			"description":  fn["description"],
			"input_schema": params,
		})
	}
	return out
}

// userContentToCC converts OpenAI user content for Command Code. Text-only
// content flattens to a plain string (the historical wire shape); when image
// parts are present the content becomes typed parts so images survive the trip.
// OpenAI image_url parts map to the Command Code image shape via ccImagePart,
// and pre-shaped image parts (the Anthropic path emits these) pass through
// verbatim. See docs/ROADMAP.md for the probe that established the accepted
// shapes.
func userContentToCC(content any) any {
	list, ok := content.([]any)
	if !ok {
		return contentToText(content)
	}
	parts := []any{}
	hasImage := false
	for _, pi := range list {
		p := getMap(pi)
		if p == nil {
			continue
		}
		switch getStr(p, "type") {
		case "text":
			if t := getStr(p, "text"); t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image_url":
			if url := getStr(getMap(p["image_url"]), "url"); url != "" {
				parts = append(parts, ccImagePart(url))
				hasImage = true
			}
		case "image":
			parts = append(parts, p)
			hasImage = true
		}
	}
	if !hasImage {
		return contentToText(content)
	}
	return parts
}

// ccImagePart builds Command Code's image part — {"type":"image","image":<data:
// or https: URL>} plus "mimeType" when knowable, matching what the current CLI
// (command-code@1.15.1) sends.
func ccImagePart(url string) map[string]any {
	part := map[string]any{"type": "image", "image": url}
	if mt := dataURLMime(url); mt != "" {
		part["mimeType"] = mt
	}
	return part
}

// dataURLMime extracts the mime type from a data: URL, or "" for other URLs.
func dataURLMime(url string) string {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, ";,"); i > 0 {
		return rest[:i]
	}
	return ""
}

// imagePartsToCC collects the image parts of an OpenAI content list as Command
// Code image parts, tolerating both image_url and pre-shaped image parts.
func imagePartsToCC(content any) []any {
	list, ok := content.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, pi := range list {
		p := getMap(pi)
		if p == nil {
			continue
		}
		switch getStr(p, "type") {
		case "image_url":
			if url := getStr(getMap(p["image_url"]), "url"); url != "" {
				out = append(out, ccImagePart(url))
			}
		case "image":
			out = append(out, p)
		}
	}
	return out
}

// contentToText flattens OpenAI message content (string or list of parts) to text.
func contentToText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, pi := range c {
			switch p := pi.(type) {
			case map[string]any:
				if getStr(p, "type") == "text" {
					parts = append(parts, getStr(p, "text"))
				}
			case string:
				parts = append(parts, p)
			}
		}
		return joinNonEmpty(parts)
	}
	return ""
}

func parseArguments(arguments any) map[string]any {
	switch a := arguments.(type) {
	case map[string]any:
		return a
	case string:
		var parsed any
		if json.Unmarshal([]byte(a), &parsed) == nil {
			if m, ok := parsed.(map[string]any); ok {
				return m
			}
		}
	}
	return map[string]any{}
}

// ── Response: shared parsing / mapping ───────────────────────────────────────

// ParseStreamEventLine parses one upstream stream line into an event, or nil to
// skip. Tolerates both raw NDJSON and data:-prefixed SSE.
func ParseStreamEventLine(line string) Event {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, ":") || strings.HasPrefix(s, "event:") {
		return nil
	}
	if strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(s[5:])
	}
	if s == "" || s == "[DONE]" {
		return nil
	}
	var parsed any
	if json.Unmarshal([]byte(s), &parsed) != nil {
		return nil
	}
	if m, ok := parsed.(map[string]any); ok {
		return m
	}
	return nil
}

func MapFinishReason(reason any) string {
	switch reason {
	case "tool-calls":
		return "tool_calls"
	case "length", "max_tokens", "max-tokens", "max_output_tokens":
		return "length"
	}
	return "stop"
}

func MapUsage(totalUsage any) map[string]any {
	m := getMap(totalUsage)
	if m == nil {
		return nil
	}
	inp := intOf(m["inputTokens"])
	out := intOf(m["outputTokens"])
	usage := map[string]any{
		"prompt_tokens":     inp,
		"completion_tokens": out,
		"total_tokens":      inp + out,
	}
	if details := getMap(m["inputTokenDetails"]); details != nil {
		if cached := intOf(details["cacheReadTokens"]); cached != 0 {
			usage["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	return usage
}

func toolInput(ev Event) map[string]any {
	for _, key := range []string{"input", "args", "arguments"} {
		if v, ok := ev[key]; ok && v != nil {
			return parseArguments(v)
		}
	}
	return map[string]any{}
}

// NowUnix returns the current Unix time in seconds.
func NowUnix() int64 { return time.Now().Unix() }

// ── small helpers (map[string]any access, like Python dict .get) ─────────────

func getStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func getList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// getOrDefault returns m[key] when it is a string, else def (mirrors dict.get).
func getOrDefault(m map[string]any, key, def string) string {
	if v, ok := m[key]; ok {
		if s, ok2 := v.(string); ok2 {
			return s
		}
	}
	return def
}

func joinNonEmpty(parts []string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "\n")
}

func intOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	}
	return 0
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// firstTruthy returns the first "truthy" value (mirrors Python's `a or b`).
func firstTruthy(vals ...any) (any, bool) {
	for _, v := range vals {
		if truthy(v) {
			return v, true
		}
	}
	return nil, false
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case float64:
		return x != 0
	case string:
		return x != ""
	case bool:
		return x
	default:
		return true
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func genToolID() string { return "call_" + randHex(12) }

func orGenToolID(id string) string {
	if id == "" {
		return genToolID()
	}
	return id
}
