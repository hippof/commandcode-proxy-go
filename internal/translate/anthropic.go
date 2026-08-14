package translate

import "strings"

// OpenAIRequestFromAnthropic adapts an Anthropic Messages request into the
// OpenAI request shape, so it can flow through BuildCCRequest unchanged.
func OpenAIRequestFromAnthropic(body map[string]any) map[string]any {
	messages := []any{}
	if sys := systemToText(body["system"]); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	for _, mi := range getList(body["messages"]) {
		if m := getMap(mi); m != nil {
			messages = append(messages, anthMessageToOpenAI(m)...)
		}
	}
	out := map[string]any{
		"model":       body["model"],
		"messages":    messages,
		"max_tokens":  body["max_tokens"],
		"temperature": body["temperature"],
	}
	if tools := anthToolsToOpenAI(body["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if eff := effortFromThinking(getMap(body["thinking"])); eff != "" {
		out["reasoning_effort"] = eff
	}
	return out
}

// effortFromThinking maps Anthropic's thinking config onto a Command Code
// reasoning effort. The budget tiers follow Claude Code's presets (think 4k /
// megathink 10k / ultrathink 32k); "high" is the ceiling because it is the
// only top tier every reasoning model accepts. Disabled or budget-less
// thinking adds nothing — Command Code then picks the model's default depth.
func effortFromThinking(t map[string]any) string {
	if getStr(t, "type") != "enabled" {
		return ""
	}
	budget, ok := toInt(t["budget_tokens"])
	if !ok {
		return ""
	}
	switch {
	case budget <= 4096:
		return "low"
	case budget <= 16384:
		return "medium"
	default:
		return "high"
	}
}

func systemToText(system any) string {
	switch s := system.(type) {
	case string:
		return s
	case []any:
		var parts []string
		for _, bi := range s {
			if b := getMap(bi); b != nil && getStr(b, "type") == "text" {
				parts = append(parts, getStr(b, "text"))
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// anthMessageToOpenAI converts one Anthropic message into one or more OpenAI
// messages. A user turn's tool_result blocks fan out into OpenAI tool messages;
// an assistant turn's tool_use blocks become tool_calls.
func anthMessageToOpenAI(m map[string]any) []any {
	role := getStr(m, "role")
	if s, ok := m["content"].(string); ok {
		return []any{map[string]any{"role": role, "content": s}}
	}
	list := getList(m["content"])
	if list == nil {
		return []any{}
	}

	if role == "assistant" {
		var textParts []string
		toolCalls := []any{}
		for _, bi := range list {
			b := getMap(bi)
			if b == nil {
				continue
			}
			switch getStr(b, "type") {
			case "text":
				textParts = append(textParts, getStr(b, "text"))
			case "tool_use":
				toolCalls = append(toolCalls, map[string]any{
					"id":   getStr(b, "id"),
					"type": "function",
					"function": map[string]any{
						"name":      getStr(b, "name"),
						"arguments": jsonString(orEmptyMap(b["input"])),
					},
				})
			}
			// thinking / redacted_thinking blocks are dropped on the way upstream.
		}
		msg := map[string]any{"role": "assistant"}
		if j := joinNonEmpty(textParts); j != "" {
			msg["content"] = j
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		return []any{msg}
	}

	// user (and any other) role
	out := []any{}
	parts := []any{}
	hasImage := false
	for _, bi := range list {
		b := getMap(bi)
		if b == nil {
			continue
		}
		switch getStr(b, "type") {
		case "text":
			if t := getStr(b, "text"); t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image":
			if part := anthImageToCC(getMap(b["source"])); part != nil {
				parts = append(parts, part)
				hasImage = true
			}
		case "tool_result":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": getStr(b, "tool_use_id"),
				"content":      toolResultContent(b["content"]),
			})
		}
	}
	switch {
	case hasImage:
		out = append(out, map[string]any{"role": "user", "content": parts})
	case len(parts) > 0: // text-only keeps the flattened-string shape
		out = append(out, map[string]any{"role": "user", "content": contentToText(parts)})
	}
	return out
}

// anthImageToCC converts an Anthropic image source into Command Code's image
// part shape (see ccImagePart). Extras like cache_control never leak upstream
// because only the source's data travels.
func anthImageToCC(src map[string]any) map[string]any {
	switch getStr(src, "type") {
	case "base64":
		mt, data := getStr(src, "media_type"), getStr(src, "data")
		if mt != "" && data != "" {
			return ccImagePart("data:" + mt + ";base64," + data)
		}
	case "url":
		if u := getStr(src, "url"); u != "" {
			return ccImagePart(u)
		}
	}
	return nil
}

// toolResultContent renders a tool_result's content for the OpenAI shape:
// plain text normally, a typed-part list when image blocks are present so
// they survive into messagesToCC (which re-emits them as a user turn).
func toolResultContent(content any) any {
	imgs := []any{}
	for _, bi := range getList(content) {
		if b := getMap(bi); b != nil && getStr(b, "type") == "image" {
			if part := anthImageToCC(getMap(b["source"])); part != nil {
				imgs = append(imgs, part)
			}
		}
	}
	text := toolResultToText(content)
	if len(imgs) == 0 {
		return text
	}
	parts := []any{}
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	return append(parts, imgs...)
}

func toolResultToText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, bi := range c {
			switch b := bi.(type) {
			case map[string]any:
				if getStr(b, "type") == "text" {
					parts = append(parts, getStr(b, "text"))
				}
			case string:
				parts = append(parts, b)
			}
		}
		return joinNonEmpty(parts)
	}
	return ""
}

func anthToolsToOpenAI(tools any) []any {
	out := []any{}
	for _, ti := range getList(tools) {
		t := getMap(ti)
		if t == nil || getStr(t, "name") == "" {
			continue
		}
		params := getMap(t["input_schema"])
		if params == nil {
			params = map[string]any{}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t["name"],
				"description": t["description"],
				"parameters":  params,
			},
		})
	}
	return out
}

// orEmptyMap returns v when it is a non-empty object, else an empty object
// (mirrors Python's `block.get("input") or {}`).
func orEmptyMap(v any) any {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		return m
	}
	return map[string]any{}
}

// ── Token counting (local estimate; no upstream call) ────────────────────────

// CountInputTokens estimates the input tokens of an Anthropic Messages request
// at roughly 4 characters per token, with a flat allowance per image block and
// a small per-message overhead. Command Code has no counting endpoint, so this
// is a local approximation — good enough for client-side context budgeting
// (Claude Code's use), not billing.
func CountInputTokens(body map[string]any) int {
	chars := len(systemToText(body["system"]))
	images := 0
	msgs := getList(body["messages"])
	for _, mi := range msgs {
		m := getMap(mi)
		if m == nil {
			continue
		}
		if s, ok := m["content"].(string); ok {
			chars += len(s)
			continue
		}
		for _, bi := range getList(m["content"]) {
			b := getMap(bi)
			if b == nil {
				continue
			}
			switch getStr(b, "type") {
			case "text":
				chars += len(getStr(b, "text"))
			case "thinking":
				chars += len(getStr(b, "thinking"))
			case "tool_use":
				chars += len(getStr(b, "name")) + len(jsonString(b["input"]))
			case "tool_result":
				chars += len(toolResultToText(b["content"]))
			case "image":
				images++
			}
		}
	}
	for _, ti := range getList(body["tools"]) {
		t := getMap(ti)
		if t == nil {
			continue
		}
		chars += len(getStr(t, "name")) + len(getStr(t, "description")) + len(jsonString(t["input_schema"]))
	}
	const charsPerToken = 4
	const imageTokens = 1500 // Anthropic's ceiling for a large image
	return (chars+charsPerToken-1)/charsPerToken + 3*len(msgs) + imageTokens*images
}

// ── Response: shared mapping ──────────────────────────────────────────────────

func stopReason(openaiFinish any) string {
	switch openaiFinish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	}
	return "end_turn"
}

func anthropicUsage(openaiUsage any) map[string]any {
	m := getMap(openaiUsage)
	if m == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	return map[string]any{
		"input_tokens":  intOf(m["prompt_tokens"]),
		"output_tokens": intOf(m["completion_tokens"]),
	}
}

func toolUseID(raw any) string {
	if s, ok := raw.(string); ok && s != "" {
		return s
	}
	return "toolu_" + randHex(12)
}

// ── Response: buffered (reuses BuildCompletion) ───────────────────────────────

// BuildMessage accumulates Command Code events into a single Anthropic Message.
// The second return is a non-nil Command Code error object when the turn produced
// nothing but an error.
func BuildMessage(first Event, next NextFunc, model, mid string) (map[string]any, map[string]any) {
	completion, ccErr := BuildCompletion(first, next, model, mid, 0)
	if ccErr != nil {
		return nil, ccErr
	}
	return MessageFromCompletion(completion, model, mid), nil
}

func MessageFromCompletion(completion map[string]any, model, mid string) map[string]any {
	var choice map[string]any
	if choices := getList(completion["choices"]); len(choices) > 0 {
		choice = getMap(choices[0])
	}
	if choice == nil {
		choice = map[string]any{}
	}
	msg := getMap(choice["message"])
	if msg == nil {
		msg = map[string]any{}
	}

	blocks := []any{}
	if rc := getStr(msg, "reasoning_content"); rc != "" {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": rc})
	}
	if c := getStr(msg, "content"); c != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": c})
	}
	for _, tci := range getList(msg["tool_calls"]) {
		tc := getMap(tci)
		if tc == nil {
			continue
		}
		fn := getMap(tc["function"])
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    toolUseID(tc["id"]),
			"name":  getStr(fn, "name"),
			"input": parseArguments(fn["arguments"]),
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}

	return map[string]any{
		"id":            mid,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   stopReason(choice["finish_reason"]),
		"stop_sequence": nil,
		"usage":         anthropicUsage(completion["usage"]),
	}
}

func EmptyMessage(mid, model string) map[string]any {
	return map[string]any{
		"id":            mid,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []any{map[string]any{"type": "text", "text": ""}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
	}
}

// ── Response: streaming (Anthropic SSE state machine) ─────────────────────────

func anthEvent(eventType string, data map[string]any) string {
	return "event: " + eventType + "\ndata: " + jsonString(data) + "\n\n"
}

func messageStartFrame(mid, model string) string {
	return anthEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            mid,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func blockStopFrame(index int) string {
	return anthEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

// StreamMessage emits Anthropic streaming SSE frames from the Command Code event
// stream. Command Code emits a flat text/reasoning/tool-call stream; Anthropic
// groups output into indexed content blocks, so this opens/closes one block as
// the active type changes.
func StreamMessage(first Event, next NextFunc, model, mid string, emit func(string) error) error {
	if err := emit(messageStartFrame(mid, model)); err != nil {
		return err
	}

	index := -1
	current := "" // "" | "text" | "thinking" | "tool_use"
	sawTool := false
	stopReasonVal := "end_turn"
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}

	ev, ok := first, true
	for ok {
		switch t, _ := ev["type"].(string); t {
		case "text-delta":
			text := getStr(ev, "text")
			if text == "" {
				break
			}
			if current != "text" {
				if current != "" {
					if err := emit(blockStopFrame(index)); err != nil {
						return err
					}
				}
				index++
				current = "text"
				if err := emit(anthEvent("content_block_start", map[string]any{
					"type": "content_block_start", "index": index,
					"content_block": map[string]any{"type": "text", "text": ""},
				})); err != nil {
					return err
				}
			}
			if err := emit(anthEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "text_delta", "text": text},
			})); err != nil {
				return err
			}
		case "reasoning-delta":
			text := getStr(ev, "text")
			if text == "" {
				break
			}
			if current != "thinking" {
				if current != "" {
					if err := emit(blockStopFrame(index)); err != nil {
						return err
					}
				}
				index++
				current = "thinking"
				if err := emit(anthEvent("content_block_start", map[string]any{
					"type": "content_block_start", "index": index,
					"content_block": map[string]any{"type": "thinking", "thinking": ""},
				})); err != nil {
					return err
				}
			}
			if err := emit(anthEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": text},
			})); err != nil {
				return err
			}
		case "tool-call":
			sawTool = true
			if current != "" {
				if err := emit(blockStopFrame(index)); err != nil {
					return err
				}
			}
			index++
			current = "tool_use"
			if err := emit(anthEvent("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    toolUseID(ev["toolCallId"]),
					"name":  getStr(ev, "toolName"),
					"input": map[string]any{},
				},
			})); err != nil {
				return err
			}
			// Command Code delivers the whole tool input at once, not streamed.
			if err := emit(anthEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": jsonString(toolInput(ev))},
			})); err != nil {
				return err
			}
		case "finish":
			stopReasonVal = stopReason(MapFinishReason(ev["finishReason"]))
			usage = anthropicUsage(MapUsage(ev["totalUsage"]))
		case "error":
			if current != "" {
				if err := emit(blockStopFrame(index)); err != nil {
					return err
				}
			}
			e := getMap(ev["error"])
			if e == nil {
				e = map[string]any{}
			}
			if err := emit(anthEvent("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": getOrDefault(e, "message", "upstream error")},
			})); err != nil {
				return err
			}
			return nil
		}
		ev, ok = next()
	}

	if current != "" {
		if err := emit(blockStopFrame(index)); err != nil {
			return err
		}
	}
	if sawTool && stopReasonVal == "end_turn" {
		stopReasonVal = "tool_use"
	}
	if err := emit(anthEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReasonVal, "stop_sequence": nil},
		"usage": usage,
	})); err != nil {
		return err
	}
	return emit(anthEvent("message_stop", map[string]any{"type": "message_stop"}))
}

func EmptyMessageStream(mid, model string, emit func(string) error) error {
	if err := emit(messageStartFrame(mid, model)); err != nil {
		return err
	}
	if err := emit(anthEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})); err != nil {
		return err
	}
	if err := emit(blockStopFrame(0)); err != nil {
		return err
	}
	if err := emit(anthEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	})); err != nil {
		return err
	}
	return emit(anthEvent("message_stop", map[string]any{"type": "message_stop"}))
}
