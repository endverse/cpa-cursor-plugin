package plugin

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"cpa-cursor/internal/cursor"
)

const sigPrefix = "cps1."

func normFormat(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "claude", "anthropic":
		return "claude"
	case "openai-response", "openai-responses", "responses":
		return "openai-response"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func seal(text string) string {
	return sigPrefix + base64.RawURLEncoding.EncodeToString([]byte(text))
}

func openSeal(sig string) (string, bool) {
	if !strings.HasPrefix(sig, sigPrefix) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sig, sigPrefix))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func Parse(format string, payload []byte, meta map[string]any) (cursor.Chat, error) {
	format = normFormat(format)
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return cursor.Chat{}, fmt.Errorf("request is not JSON")
	}
	var chat cursor.Chat
	var err error
	switch format {
	case "claude":
		chat, err = parseClaude(body)
	case "openai-response":
		chat, err = parseResponses(body)
	default:
		return cursor.Chat{}, fmt.Errorf("unsupported format %q", format)
	}
	if err != nil {
		return cursor.Chat{}, err
	}
	if chat.Model == "" {
		chat.Model = "auto"
	}
	if chat.Effort == "" {
		chat.Effort = effortOf(body, meta)
	}
	if !chat.Fast {
		chat.Fast = fastOf(body, meta)
	}
	if chat.ConvKey == "" {
		chat.ConvKey = sessionKey(body, meta)
	}
	return chat, nil
}

func sessionKey(body, meta map[string]any) string {
	if s := strings.TrimSpace(asString(body["prompt_cache_key"])); s != "" {
		return s
	}
	for _, k := range []string{"canonical_session_id", "derived_session_id"} {
		if s := strings.TrimSpace(asString(meta[k])); s != "" {
			return s
		}
	}
	return ""
}

func effortOf(body, meta map[string]any) string {
	if s := asString(meta["reasoning_effort"]); s != "" {
		return normEffort(s)
	}
	if r, _ := body["reasoning"].(map[string]any); r != nil {
		if s := asString(r["effort"]); s != "" {
			return normEffort(s)
		}
	}
	if s := asString(body["reasoning_effort"]); s != "" {
		return normEffort(s)
	}
	if oc, _ := body["output_config"].(map[string]any); oc != nil {
		if s := asString(oc["effort"]); s != "" {
			return normEffort(s)
		}
	}
	if th, _ := body["thinking"].(map[string]any); th != nil {
		switch asString(th["type"]) {
		case "disabled":
			return "none"
		case "enabled", "adaptive":
			return "high"
		}
	}
	return ""
}

func normEffort(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "extra-high", "extra_high", "x-high":
		return "xhigh"
	case "off":
		return "none"
	default:
		return s
	}
}

func fastOf(body, meta map[string]any) bool {
	s := asString(body["service_tier"])
	if s == "" {
		s = asString(meta["service_tier"])
	}
	return s == "priority" || s == "fast"
}

func parseClaude(body map[string]any) (cursor.Chat, error) {
	chat := cursor.Chat{Model: strings.TrimPrefix(asString(body["model"]), "cursor/")}
	if sys := body["system"]; sys != nil {
		if text := contentText(sys); text != "" {
			chat.Messages = append(chat.Messages, cursor.Message{Role: "system", Parts: []cursor.Part{{Type: "text", Text: text}}})
		}
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		return chat, fmt.Errorf("messages is required")
	}
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		role := asString(m["role"])
		if role == "" {
			role = "user"
		}
		chat.Messages = append(chat.Messages, claudeMessage(role, m["content"])...)
	}
	chat.Tools = claudeTools(body["tools"], body["tool_choice"])
	return chat, nil
}

func claudeMessage(role string, content any) []cursor.Message {
	if s, ok := content.(string); ok {
		if s == "" {
			return nil
		}
		return []cursor.Message{{Role: role, Parts: []cursor.Part{{Type: "text", Text: s}}}}
	}
	parts, _ := content.([]any)
	if role == "assistant" {
		msg := cursor.Message{Role: "assistant"}
		for _, raw := range parts {
			p, _ := raw.(map[string]any)
			if p == nil {
				continue
			}
			switch asString(p["type"]) {
			case "thinking":
				text := asString(p["thinking"])
				if text == "" {
					text, _ = openSeal(asString(p["signature"]))
				}
				if text != "" {
					msg.Parts = append(msg.Parts, cursor.Part{Type: "reasoning", Text: text})
				}
			case "text":
				if t := asString(p["text"]); t != "" {
					msg.Parts = append(msg.Parts, cursor.Part{Type: "text", Text: t})
				}
			case "tool_use":
				args, _ := json.Marshal(p["input"])
				if p["input"] == nil {
					args = []byte("{}")
				}
				msg.Parts = append(msg.Parts, cursor.Part{Type: "tool-call", ToolID: asString(p["id"]), Name: asString(p["name"]), Args: string(args)})
			}
		}
		if len(msg.Parts) == 0 {
			return nil
		}
		return []cursor.Message{msg}
	}
	var out []cursor.Message
	var tools []cursor.Part
	var user []cursor.Part
	flushTools := func() {
		if len(tools) == 0 {
			return
		}
		out = append(out, cursor.Message{Role: "tool", Parts: tools})
		tools = nil
	}
	for _, raw := range parts {
		p, _ := raw.(map[string]any)
		if p == nil {
			continue
		}
		switch asString(p["type"]) {
		case "tool_result":
			tools = append(tools, cursor.Part{
				Type: "tool-result", ToolID: asString(p["tool_use_id"]),
				Text: contentText(p["content"]), IsError: asBool(p["is_error"]),
			})
		case "image":
			flushTools()
			user = append(user, imagePart(p["source"]))
		default:
			flushTools()
			if t := asString(p["text"]); t != "" {
				user = append(user, cursor.Part{Type: "text", Text: t})
			}
		}
	}
	flushTools()
	if len(user) > 0 {
		out = append(out, cursor.Message{Role: "user", Parts: user})
	}
	return out
}

func imagePart(source any) cursor.Part {
	s, _ := source.(map[string]any)
	if s == nil {
		return cursor.Part{Type: "text", Text: "[attachment image]"}
	}
	switch asString(s["type"]) {
	case "base64":
		raw, err := base64.StdEncoding.DecodeString(asString(s["data"]))
		if err != nil {
			raw, _ = base64.RawStdEncoding.DecodeString(asString(s["data"]))
		}
		if len(raw) == 0 {
			return cursor.Part{Type: "text", Text: "[attachment image]"}
		}
		return cursor.Part{Type: "image", MIME: asString(s["media_type"]), Image: raw}
	default:
		u := asString(s["url"])
		if u == "" {
			u = asString(s["data"])
		}
		return cursor.Part{Type: "image", ImageURL: u}
	}
}

func claudeTools(raw any, choice any) []cursor.Tool {
	if choice == "none" {
		return nil
	}
	if m, _ := choice.(map[string]any); m != nil && asString(m["type"]) == "none" {
		return nil
	}
	only := ""
	if m, _ := choice.(map[string]any); m != nil && (asString(m["type"]) == "tool" || asString(m["type"]) == "function") {
		only = asString(m["name"])
	}
	list, _ := raw.([]any)
	var out []cursor.Tool
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil || isWeb(asString(m["type"])) {
			continue
		}
		name := asString(m["name"])
		if name == "" {
			continue
		}
		if only != "" && name != only {
			continue
		}
		schema := m["input_schema"]
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		text, _ := json.Marshal(schema)
		out = append(out, cursor.Tool{Name: name, Description: asString(m["description"]), Schema: schema, SchemaText: string(text)})
	}
	return out
}

func parseResponses(body map[string]any) (cursor.Chat, error) {
	chat := cursor.Chat{Model: strings.TrimPrefix(asString(body["model"]), "cursor/")}
	if ins := asString(body["instructions"]); ins != "" {
		chat.Messages = append(chat.Messages, cursor.Message{Role: "system", Parts: []cursor.Part{{Type: "text", Text: ins}}})
	}
	switch input := body["input"].(type) {
	case string:
		if input != "" {
			chat.Messages = append(chat.Messages, cursor.Message{Role: "user", Parts: []cursor.Part{{Type: "text", Text: input}}})
		}
	case []any:
		chat.Messages = append(chat.Messages, responsesItems(input)...)
	}
	if len(chat.Messages) == 0 {
		return chat, fmt.Errorf("input is required")
	}
	chat.Tools = responsesTools(body["tools"], body["tool_choice"])
	return chat, nil
}

func responsesItems(items []any) []cursor.Message {
	var out []cursor.Message
	var cur *cursor.Message
	flush := func() {
		if cur == nil {
			return
		}
		if len(cur.Parts) > 0 {
			out = append(out, *cur)
		}
		cur = nil
	}
	assist := func() *cursor.Message {
		if cur == nil || cur.Role != "assistant" {
			flush()
			cur = &cursor.Message{Role: "assistant"}
		}
		return cur
	}
	for _, raw := range items {
		switch item := raw.(type) {
		case string:
			flush()
			out = append(out, cursor.Message{Role: "user", Parts: []cursor.Part{{Type: "text", Text: item}}})
		case map[string]any:
			typ := asString(item["type"])
			switch typ {
			case "function_call":
				args := asString(item["arguments"])
				if args == "" {
					args = "{}"
				}
				assist().Parts = append(assist().Parts, cursor.Part{Type: "tool-call", ToolID: firstNonEmpty(asString(item["call_id"]), asString(item["id"])), Name: asString(item["name"]), Args: args})
			case "function_call_output":
				flush()
				out = append(out, cursor.Message{Role: "tool", Parts: []cursor.Part{{
					Type: "tool-result", ToolID: asString(item["call_id"]), Text: contentText(item["output"]),
				}}})
			case "reasoning":
				text := responsesReasoning(item)
				if text != "" {
					assist().Parts = append(assist().Parts, cursor.Part{Type: "reasoning", Text: text})
				}
			case "message", "":
				role := asString(item["role"])
				if role == "" {
					role = "user"
				}
				if role == "system" || role == "developer" {
					flush()
					if t := contentText(item["content"]); t != "" {
						out = append(out, cursor.Message{Role: "system", Parts: []cursor.Part{{Type: "text", Text: t}}})
					}
					continue
				}
				if role == "assistant" {
					m := assist()
					m.Parts = append(m.Parts, responsesContent(item["content"])...)
					continue
				}
				flush()
				parts := responsesContent(item["content"])
				if len(parts) > 0 {
					out = append(out, cursor.Message{Role: "user", Parts: parts})
				}
			default:
				if isWeb(typ) {
					continue
				}
			}
		}
	}
	flush()
	return out
}

func responsesReasoning(item map[string]any) string {
	var b strings.Builder
	if summary, _ := item["summary"].([]any); len(summary) > 0 {
		for _, raw := range summary {
			m, _ := raw.(map[string]any)
			if m == nil {
				continue
			}
			if t := asString(m["text"]); t != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(t)
			}
		}
	}
	if b.Len() == 0 {
		if t, ok := openSeal(asString(item["encrypted_content"])); ok {
			return t
		}
	}
	return b.String()
}

func responsesContent(content any) []cursor.Part {
	if s, ok := content.(string); ok {
		if s == "" {
			return nil
		}
		return []cursor.Part{{Type: "text", Text: s}}
	}
	list, _ := content.([]any)
	var out []cursor.Part
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		switch asString(m["type"]) {
		case "input_text", "output_text", "text":
			if t := asString(m["text"]); t != "" {
				out = append(out, cursor.Part{Type: "text", Text: t})
			}
		case "input_image":
			out = append(out, responsesImage(m))
		case "refusal":
			if t := asString(m["refusal"]); t != "" {
				out = append(out, cursor.Part{Type: "text", Text: t})
			}
		}
	}
	return out
}

func responsesImage(m map[string]any) cursor.Part {
	switch u := m["image_url"].(type) {
	case string:
		return dataImage(u)
	case map[string]any:
		return dataImage(asString(u["url"]))
	default:
		return cursor.Part{Type: "text", Text: "[attachment image]"}
	}
}

func dataImage(u string) cursor.Part {
	if strings.HasPrefix(u, "data:") {
		if i := strings.Index(u, ","); i > 0 {
			meta := u[5:i]
			mime := meta
			if semi := strings.Index(mime, ";"); semi >= 0 {
				mime = mime[:semi]
			}
			raw, err := base64.StdEncoding.DecodeString(u[i+1:])
			if err != nil || len(raw) == 0 {
				return cursor.Part{Type: "image", ImageURL: u}
			}
			return cursor.Part{Type: "image", MIME: mime, Image: raw}
		}
	}
	return cursor.Part{Type: "image", ImageURL: u}
}

func responsesTools(raw, choice any) []cursor.Tool {
	if choice == "none" {
		return nil
	}
	if m, _ := choice.(map[string]any); m != nil && asString(m["type"]) == "none" {
		return nil
	}
	only := ""
	if m, _ := choice.(map[string]any); m != nil {
		only = asString(m["name"])
	}
	list, _ := raw.([]any)
	var out []cursor.Tool
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil || isWeb(asString(m["type"])) {
			continue
		}
		name := asString(m["name"])
		desc := asString(m["description"])
		schema := m["parameters"]
		if fn, _ := m["function"].(map[string]any); fn != nil {
			if name == "" {
				name = asString(fn["name"])
			}
			if desc == "" {
				desc = asString(fn["description"])
			}
			if schema == nil {
				schema = fn["parameters"]
			}
		}
		if name == "" || (only != "" && name != only) {
			continue
		}
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		text, _ := json.Marshal(schema)
		out = append(out, cursor.Tool{Name: name, Description: desc, Schema: schema, SchemaText: string(text)})
	}
	return out
}

func isWeb(typ string) bool {
	t := strings.ToLower(typ)
	return t == "web_search" || strings.HasPrefix(t, "web_search_")
}

func contentText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var b strings.Builder
		for _, item := range x {
			switch p := item.(type) {
			case string:
				b.WriteString(p)
			case map[string]any:
				if t := asString(p["text"]); t != "" {
					b.WriteString(t)
				} else if t := asString(p["content"]); t != "" {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	case map[string]any:
		if t := asString(x["text"]); t != "" {
			return t
		}
		return contentText(x["content"])
	default:
		return ""
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func randID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

type gathered struct {
	reasoning string
	text      string
	tools     []cursor.ToolCall
	usage     *cursor.Usage
	stop      string
	err       *cursor.StatusError
}

func gather(events <-chan cursor.Event) gathered {
	var g gathered
	var reasoning, text strings.Builder
	for ev := range events {
		if ev.Err != nil {
			g.err = ev.Err
			return g
		}
		if ev.Reasoning != "" {
			reasoning.WriteString(ev.Reasoning)
		}
		if ev.Text != "" {
			text.WriteString(ev.Text)
		}
		if ev.Tool != nil {
			g.tools = append(g.tools, *ev.Tool)
		}
		if ev.Stop != "" {
			g.stop = ev.Stop
			g.usage = ev.Usage
		}
	}
	g.reasoning = reasoning.String()
	g.text = text.String()
	return g
}

func Collect(format, model string, events <-chan cursor.Event) ([]byte, error) {
	g := gather(events)
	if g.err != nil && g.text == "" && g.reasoning == "" && len(g.tools) == 0 {
		return nil, g.err
	}
	switch normFormat(format) {
	case "openai-response":
		return json.Marshal(responsesObject(model, g))
	default:
		return json.Marshal(claudeObject(model, g))
	}
}

func claudeObject(model string, g gathered) map[string]any {
	var content []any
	if g.reasoning != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": g.reasoning, "signature": seal(g.reasoning)})
	}
	if g.text != "" {
		content = append(content, map[string]any{"type": "text", "text": g.text})
	}
	for _, t := range g.tools {
		var input any
		if json.Unmarshal([]byte(t.Args), &input) != nil || input == nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{"type": "tool_use", "id": t.ID, "name": t.Name, "input": input})
	}
	if content == nil {
		content = []any{}
	}
	stop := "end_turn"
	if g.stop == "tool_calls" || len(g.tools) > 0 {
		stop = "tool_use"
	}
	return map[string]any{
		"id": randID("msg_"), "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stop, "stop_sequence": nil, "usage": claudeUsage(g.usage),
	}
}

func claudeUsage(u *cursor.Usage) map[string]any {
	if u == nil {
		u = &cursor.Usage{}
	}
	return map[string]any{
		"input_tokens":                u.Input,
		"output_tokens":               u.Output,
		"cache_read_input_tokens":     u.CacheRead,
		"cache_creation_input_tokens": u.CacheWrite,
	}
}

func responsesObject(model string, g gathered) map[string]any {
	var output []any
	if g.reasoning != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "id": randID("rs_"),
			"summary":           []any{map[string]any{"type": "summary_text", "text": g.reasoning}},
			"encrypted_content": seal(g.reasoning),
		})
	}
	if g.text != "" {
		output = append(output, map[string]any{
			"type": "message", "id": randID("msg_"), "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": g.text, "annotations": []any{}}},
		})
	}
	for _, t := range g.tools {
		output = append(output, map[string]any{
			"type": "function_call", "id": randID("fc_"), "call_id": t.ID, "name": t.Name,
			"arguments": t.Args, "status": "completed",
		})
	}
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"id": randID("resp_"), "object": "response", "status": "completed", "model": model,
		"output": output, "usage": responsesUsage(g.usage),
	}
}

func responsesUsage(u *cursor.Usage) map[string]any {
	if u == nil {
		u = &cursor.Usage{}
	}
	in := u.Input + u.CacheRead + u.CacheWrite
	return map[string]any{
		"input_tokens":          in,
		"output_tokens":         u.Output,
		"total_tokens":          in + u.Output,
		"input_tokens_details":  map[string]any{"cached_tokens": u.CacheRead},
		"output_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning},
	}
}

func Stream(format, model string, events <-chan cursor.Event, emit func([]byte) error) error {
	if normFormat(format) == "openai-response" {
		return streamResponses(model, events, emit)
	}
	return streamClaude(model, events, emit)
}

func sse(event string, v any) []byte {
	raw, _ := json.Marshal(v)
	return []byte("event: " + event + "\ndata: " + string(raw) + "\n\n")
}

func streamClaude(model string, events <-chan cursor.Event, emit func([]byte) error) error {
	id := randID("msg_")
	if err := emit(sse("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil, "usage": claudeUsage(nil),
		},
	})); err != nil {
		return err
	}
	index := 0
	open := ""
	var reasoning strings.Builder
	var sawTool bool
	var usage *cursor.Usage
	stop := "end_turn"
	closeBlock := func() error {
		if open == "thinking" {
			if err := emit(sse("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "signature_delta", "signature": seal(reasoning.String())},
			})); err != nil {
				return err
			}
		}
		if open != "" {
			if err := emit(sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})); err != nil {
				return err
			}
			index++
			open = ""
		}
		return nil
	}
	for ev := range events {
		if ev.Err != nil {
			_ = emit(sse("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": ev.Err.Error()}}))
			return ev.Err
		}
		if ev.Reasoning != "" {
			if open != "thinking" {
				if err := closeBlock(); err != nil {
					return err
				}
				if err := emit(sse("content_block_start", map[string]any{
					"type": "content_block_start", "index": index,
					"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
				})); err != nil {
					return err
				}
				open = "thinking"
			}
			reasoning.WriteString(ev.Reasoning)
			if err := emit(sse("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Reasoning},
			})); err != nil {
				return err
			}
		}
		if ev.Text != "" {
			if open != "text" {
				if err := closeBlock(); err != nil {
					return err
				}
				if err := emit(sse("content_block_start", map[string]any{
					"type": "content_block_start", "index": index,
					"content_block": map[string]any{"type": "text", "text": ""},
				})); err != nil {
					return err
				}
				open = "text"
			}
			if err := emit(sse("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "text_delta", "text": ev.Text},
			})); err != nil {
				return err
			}
		}
		if ev.Tool != nil {
			sawTool = true
			if err := closeBlock(); err != nil {
				return err
			}
			if err := emit(sse("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "tool_use", "id": ev.Tool.ID, "name": ev.Tool.Name, "input": map[string]any{}},
			})); err != nil {
				return err
			}
			if err := emit(sse("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Tool.Args},
			})); err != nil {
				return err
			}
			open = "tool"
			if err := closeBlock(); err != nil {
				return err
			}
		}
		if ev.Stop != "" {
			usage = ev.Usage
			if ev.Stop == "tool_calls" {
				stop = "tool_use"
			}
		}
	}
	if sawTool {
		stop = "tool_use"
	}
	if err := closeBlock(); err != nil {
		return err
	}
	if err := emit(sse("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": claudeUsage(usage)["output_tokens"]},
	})); err != nil {
		return err
	}
	return emit(sse("message_stop", map[string]any{"type": "message_stop"}))
}

func streamResponses(model string, events <-chan cursor.Event, emit func([]byte) error) error {
	id := randID("resp_")
	base := map[string]any{
		"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{},
	}
	if err := emit(sse("response.created", map[string]any{"type": "response.created", "response": base})); err != nil {
		return err
	}
	if err := emit(sse("response.in_progress", map[string]any{"type": "response.in_progress", "response": base})); err != nil {
		return err
	}
	index := 0
	open := ""
	openID := ""
	var reasoning, text strings.Builder
	var tools []cursor.ToolCall
	var usage *cursor.Usage
	var output []any
	closeItem := func() error {
		switch open {
		case "reasoning":
			item := map[string]any{
				"type": "reasoning", "id": openID,
				"summary":           []any{map[string]any{"type": "summary_text", "text": reasoning.String()}},
				"encrypted_content": seal(reasoning.String()),
			}
			output = append(output, item)
			if err := emit(sse("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": index, "item": item,
			})); err != nil {
				return err
			}
			index++
		case "text":
			item := map[string]any{
				"type": "message", "id": openID, "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text.String(), "annotations": []any{}}},
			}
			output = append(output, item)
			if err := emit(sse("response.output_text.done", map[string]any{
				"type": "response.output_text.done", "output_index": index, "content_index": 0, "text": text.String(),
			})); err != nil {
				return err
			}
			if err := emit(sse("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": index, "item": item,
			})); err != nil {
				return err
			}
			index++
		}
		open = ""
		return nil
	}
	for ev := range events {
		if ev.Err != nil {
			_ = emit(sse("error", map[string]any{"type": "error", "error": map[string]any{"message": ev.Err.Error()}}))
			return ev.Err
		}
		if ev.Reasoning != "" {
			if open != "reasoning" {
				if err := closeItem(); err != nil {
					return err
				}
				openID = randID("rs_")
				item := map[string]any{"type": "reasoning", "id": openID, "summary": []any{}}
				if err := emit(sse("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": index, "item": item,
				})); err != nil {
					return err
				}
				open = "reasoning"
			}
			reasoning.WriteString(ev.Reasoning)
			if err := emit(sse("response.reasoning_summary_text.delta", map[string]any{
				"type": "response.reasoning_summary_text.delta", "output_index": index, "summary_index": 0, "delta": ev.Reasoning,
			})); err != nil {
				return err
			}
		}
		if ev.Text != "" {
			if open != "text" {
				if err := closeItem(); err != nil {
					return err
				}
				openID = randID("msg_")
				item := map[string]any{"type": "message", "id": openID, "role": "assistant", "status": "in_progress", "content": []any{}}
				if err := emit(sse("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": index, "item": item,
				})); err != nil {
					return err
				}
				if err := emit(sse("response.content_part.added", map[string]any{
					"type": "response.content_part.added", "output_index": index, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				})); err != nil {
					return err
				}
				open = "text"
			}
			text.WriteString(ev.Text)
			if err := emit(sse("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "output_index": index, "content_index": 0, "delta": ev.Text,
			})); err != nil {
				return err
			}
		}
		if ev.Tool != nil {
			tools = append(tools, *ev.Tool)
			if err := closeItem(); err != nil {
				return err
			}
			item := map[string]any{
				"type": "function_call", "id": randID("fc_"), "call_id": ev.Tool.ID, "name": ev.Tool.Name,
				"arguments": "", "status": "in_progress",
			}
			if err := emit(sse("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": index, "item": item,
			})); err != nil {
				return err
			}
			if err := emit(sse("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "output_index": index, "delta": ev.Tool.Args,
			})); err != nil {
				return err
			}
			done := map[string]any{
				"type": "function_call", "id": item["id"], "call_id": ev.Tool.ID, "name": ev.Tool.Name,
				"arguments": ev.Tool.Args, "status": "completed",
			}
			output = append(output, done)
			if err := emit(sse("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": index, "item": done,
			})); err != nil {
				return err
			}
			index++
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
	}
	if err := closeItem(); err != nil {
		return err
	}
	completed := map[string]any{
		"id": id, "object": "response", "status": "completed", "model": model,
		"output": output, "usage": responsesUsage(usage),
	}
	return emit(sse("response.completed", map[string]any{"type": "response.completed", "response": completed}))
}
