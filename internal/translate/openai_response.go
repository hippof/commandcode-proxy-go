package translate

import "strings"

func chunkFrame(cid string, created int64, model string, delta map[string]any, finishReason any) string {
	return "data: " + jsonString(map[string]any{
		"id":      cid,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	}) + "\n\n"
}

func usageChunkFrame(cid string, created int64, model string, usage map[string]any) string {
	return "data: " + jsonString(map[string]any{
		"id":      cid,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{},
		"usage":   usage,
	}) + "\n\n"
}

// StreamCompletion emits OpenAI chat.completion.chunk SSE frames from the
// Command Code event stream (first is the already-peeked decisive event), calling
// emit for each frame. Returns emit's first error, if any.
func StreamCompletion(first Event, next NextFunc, model, cid string, created int64, includeUsage bool, emit func(string) error) error {
	if err := emit(chunkFrame(cid, created, model, map[string]any{"role": "assistant"}, nil)); err != nil {
		return err
	}
	toolIndex := 0
	sawTool := false
	var finishReason any = "stop"
	var usage map[string]any

	ev, ok := first, true
loop:
	for ok {
		switch t, _ := ev["type"].(string); t {
		case "text-delta":
			if text := getStr(ev, "text"); text != "" {
				if err := emit(chunkFrame(cid, created, model, map[string]any{"content": text}, nil)); err != nil {
					return err
				}
			}
		case "reasoning-delta":
			if text := getStr(ev, "text"); text != "" {
				if err := emit(chunkFrame(cid, created, model, map[string]any{"reasoning_content": text}, nil)); err != nil {
					return err
				}
			}
		case "tool-call":
			sawTool = true
			tc := map[string]any{
				"index": toolIndex,
				"id":    orGenToolID(getStr(ev, "toolCallId")),
				"type":  "function",
				"function": map[string]any{
					"name":      getStr(ev, "toolName"),
					"arguments": jsonString(toolInput(ev)),
				},
			}
			toolIndex++
			if err := emit(chunkFrame(cid, created, model, map[string]any{"tool_calls": []any{tc}}, nil)); err != nil {
				return err
			}
		case "finish":
			finishReason = MapFinishReason(ev["finishReason"])
			usage = MapUsage(ev["totalUsage"])
		case "error":
			e := getMap(ev["error"])
			if e == nil {
				e = map[string]any{}
			}
			frame := "data: " + jsonString(map[string]any{
				"error": map[string]any{
					"message": getOrDefault(e, "message", "upstream error"),
					"type":    "upstream_error",
					"code":    e["code"],
				},
			}) + "\n\n"
			if err := emit(frame); err != nil {
				return err
			}
			finishReason = nil // error already terminated the turn
			break loop
		}
		ev, ok = next()
	}

	if finishReason != nil {
		if sawTool && finishReason == "stop" {
			finishReason = "tool_calls"
		}
		if err := emit(chunkFrame(cid, created, model, map[string]any{}, finishReason)); err != nil {
			return err
		}
	}
	if includeUsage && usage != nil {
		if err := emit(usageChunkFrame(cid, created, model, usage)); err != nil {
			return err
		}
	}
	return emit("data: [DONE]\n\n")
}

// BuildCompletion accumulates Command Code events into a single chat.completion.
// The second return is a non-nil Command Code error object when the turn produced
// nothing usable but an error (the caller surfaces it as an HTTP error).
func BuildCompletion(first Event, next NextFunc, model, cid string, created int64) (map[string]any, map[string]any) {
	var textParts, reasoningParts []string
	toolCalls := []any{}
	var finishReason any = "stop"
	var usage, ccErr map[string]any

	ev, ok := first, true
loop:
	for ok {
		switch t, _ := ev["type"].(string); t {
		case "text-delta":
			textParts = append(textParts, getStr(ev, "text"))
		case "reasoning-delta":
			reasoningParts = append(reasoningParts, getStr(ev, "text"))
		case "tool-call":
			toolCalls = append(toolCalls, map[string]any{
				"id":   orGenToolID(getStr(ev, "toolCallId")),
				"type": "function",
				"function": map[string]any{
					"name":      getStr(ev, "toolName"),
					"arguments": jsonString(toolInput(ev)),
				},
			})
		case "finish":
			finishReason = MapFinishReason(ev["finishReason"])
			usage = MapUsage(ev["totalUsage"])
		case "error":
			ccErr = getMap(ev["error"])
			if ccErr == nil {
				ccErr = map[string]any{}
			}
			break loop
		}
		ev, ok = next()
	}

	message := map[string]any{"role": "assistant"}
	if content := strings.Join(textParts, ""); content != "" {
		message["content"] = content
	} else {
		message["content"] = nil
	}
	if len(reasoningParts) > 0 {
		message["reasoning_content"] = strings.Join(reasoningParts, "")
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if finishReason == "stop" {
			finishReason = "tool_calls"
		}
	}

	completion := map[string]any{
		"id":      cid,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
	}
	if usage != nil {
		completion["usage"] = usage
	}
	if ccErr != nil && len(textParts) == 0 && len(toolCalls) == 0 {
		return completion, ccErr
	}
	return completion, nil
}

func EmptyCompletion(cid string, created int64, model string) map[string]any {
	return map[string]any{
		"id":      cid,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": ""},
			"finish_reason": "stop",
		}},
	}
}

func EmptyStream(cid string, created int64, model string, emit func(string) error) error {
	if err := emit(chunkFrame(cid, created, model, map[string]any{"role": "assistant"}, nil)); err != nil {
		return err
	}
	if err := emit(chunkFrame(cid, created, model, map[string]any{}, "stop")); err != nil {
		return err
	}
	return emit("data: [DONE]\n\n")
}
