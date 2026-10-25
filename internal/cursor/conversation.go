package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"strings"
)

const (
	closeCall = "<" + "/call>"
	closeCat  = "<" + "/dynamic_tool_catalog>"
)

type Tool struct {
	Name        string
	Description string
	Schema      any
	SchemaText  string
}

type Part struct {
	Type     string
	Text     string
	ToolID   string
	Name     string
	Args     string
	IsError  bool
	MIME     string
	Image    []byte
	ImageURL string
}

type Message struct {
	Role  string
	Parts []Part
}

type Chat struct {
	Model    string
	Effort   string
	Fast     bool
	Messages []Message
	Tools    []Tool
	ConvKey  string
}

type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
	Reasoning  int
}

type ToolCall struct {
	ID   string
	Name string
	Args string
}

type Event struct {
	Text      string
	Reasoning string
	Tool      *ToolCall
	Stop      string
	Usage     *Usage
	Err       *StatusError
}

type transcript struct {
	order  [][]byte
	convID string
	last   string
	est    int
}

func goosName() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return runtime.GOOS
}

func envMessage() []byte {
	return (&pb{}).str(1, goosName()).str(2, os.TempDir()).str(10, "UTC").done()
}

func toolDef(t Tool) []byte {
	schema := t.Schema
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	d := (&pb{}).str(1, t.Name)
	if t.Description != "" {
		d.str(2, t.Description)
	}
	text := t.SchemaText
	if text == "" {
		raw, _ := json.Marshal(schema)
		text = string(raw)
	}
	d.embed(3, pbValue(schema))
	return d.str(4, toolNS).str(5, t.Name).str(6, text).done()
}

func catalogText(tools []Tool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<dynamic_tool_catalog>\nThe MCP namespace \"" + toolNS + "\" has the tools below. They are reached only through `" + callTool + "`, never as tools of their own. Each entry is the exact call to make; its arguments must match the schema given. The schemas are complete, so there is no need to call `GetDynamicTools` first.\n")
	for _, t := range tools {
		b.WriteString("<call>" + callTool + "({\"namespace\":\"" + toolNS + "\",\"toolName\":\"" + t.Name + "\",\"arguments\":{...}})\n")
		b.WriteString(t.Description)
		b.WriteString("\narguments schema: ")
		b.WriteString(t.SchemaText)
		b.WriteString("\n")
		b.WriteString(closeCall)
		b.WriteString("\n")
	}
	b.WriteString(closeCat)
	return b.String()
}

func callID(id string) string {
	if strings.HasPrefix(id, "call_") {
		return strings.ReplaceAll(id, "__fc_", "\nfc_")
	}
	return id
}

func toolResult(id, text string, isError bool) map[string]any {
	var res any = text
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) == nil {
		switch parsed.(type) {
		case map[string]any, []any:
			res = parsed
		}
	}
	out := map[string]any{
		"type":       "tool-result",
		"toolCallId": callID(id),
		"toolName":   callTool,
		"result":     res,
		"experimental_content": []any{
			map[string]any{"type": "text", "text": text},
		},
	}
	if isError {
		out["isError"] = true
	}
	return out
}

func textOf(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func reasoningOf(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "reasoning" && strings.TrimSpace(p.Text) != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func (c Chat) transcript() transcript {
	var order [][]byte
	add := func(m any) {
		raw, _ := json.Marshal(m)
		order = append(order, raw)
	}
	var system []string
	for _, m := range c.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if t := textOf(m.Parts); t != "" {
				system = append(system, t)
			}
		}
	}
	sys := strings.Join(system, "\n\n") + catalogText(c.Tools)
	size := len(sys)
	if strings.TrimSpace(sys) != "" {
		add(map[string]any{"role": "system", "content": sys})
	}
	var pending []string
	var results []map[string]any
	answer := func() {
		for _, id := range pending {
			results = append(results, toolResult(id, noResult, true))
		}
		pending = nil
		if len(results) > 0 {
			add(map[string]any{"role": "tool", "content": results})
			results = nil
		}
	}
	last := "."
	var firstUser []byte
	for _, m := range c.Messages {
		switch m.Role {
		case "system", "developer":
			continue
		case "tool":
			for _, p := range m.Parts {
				idx := indexOf(pending, p.ToolID)
				if idx < 0 {
					continue
				}
				pending = append(pending[:idx], pending[idx+1:]...)
				size += len(p.Text)
				results = append(results, toolResult(p.ToolID, p.Text, p.IsError))
			}
		case "assistant":
			answer()
			var content []any
			if reasoning := reasoningOf(m.Parts); reasoning != "" {
				content = append(content, map[string]any{"type": "reasoning", "text": reasoning})
				size += len(reasoning)
			}
			if text := textOf(m.Parts); text != "" {
				content = append(content, map[string]any{"type": "text", "text": text})
				size += len(text)
			}
			for _, p := range m.Parts {
				if p.Type != "tool-call" {
					continue
				}
				var args any
				if json.Unmarshal([]byte(p.Args), &args) != nil || args == nil {
					args = map[string]any{}
				}
				size += len(p.Args) + len(p.Name)
				content = append(content, map[string]any{
					"type": "tool-call", "toolCallId": callID(p.ToolID), "toolName": callTool,
					"args": map[string]any{"namespace": toolNS, "toolName": p.Name, "arguments": args},
				})
				pending = append(pending, p.ToolID)
			}
			if len(content) > 0 {
				add(map[string]any{"role": "assistant", "content": content})
			}
		default:
			var content []any
			first := ""
			for _, p := range m.Parts {
				switch p.Type {
				case "", "text":
					if p.Text == "" {
						continue
					}
					content = append(content, map[string]any{"type": "text", "text": p.Text})
					size += len(p.Text)
					if first == "" {
						first = p.Text
					}
				case "image":
					if len(p.Image) > 0 {
						mime := p.MIME
						if mime == "" {
							mime = "image/png"
						}
						content = append(content, map[string]any{
							"type": "image", "mimeType": mime,
							"image": map[string]any{"__type": "Uint8Array", "hex": hex.EncodeToString(p.Image)},
						})
					} else if p.ImageURL != "" {
						content = append(content, map[string]any{"type": "text", "text": "[attachment image: " + p.ImageURL + "]"})
					}
				}
			}
			answer()
			if first != "" {
				last = first
			}
			if len(content) > 0 {
				raw, _ := json.Marshal(map[string]any{"role": "user", "content": content})
				if firstUser == nil {
					firstUser = raw
				}
				order = append(order, raw)
			}
		}
	}
	answer()
	for _, t := range c.Tools {
		size += len(t.Name) + len(t.Description) + len(t.SchemaText)
	}
	convID := ""
	if key := strings.TrimSpace(c.ConvKey); key != "" {
		sum := sha256.Sum256([]byte("cursor conversation\x00" + key + "\x00" + string(firstUser)))
		convID = uuidOf(sum[:16])
	}
	if size < 0 {
		size = 0
	}
	return transcript{order: order, convID: convID, last: last, est: size / 4}
}

func uuidOf(b []byte) string {
	id := append([]byte(nil), b[:16]...)
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	h := hex.EncodeToString(id)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func buildRun(order [][]byte, lastUser string, tools []Tool, model, conv string, maxMode bool, params []Param) (first []byte, blobs map[string][]byte) {
	blobs = map[string][]byte{}
	put := func(b []byte) []byte {
		sum := sha256.Sum256(b)
		blobs[hex.EncodeToString(sum[:])] = append([]byte(nil), b...)
		return sum[:]
	}
	state := &pb{}
	for _, m := range order {
		state.bytes(1, put(m))
	}
	mid := newID()
	user := (&pb{}).str(1, lastUser).str(2, mid).varint(4, 1).done()
	turn := (&pb{}).embed(1, (&pb{}).bytes(1, put(user)).str(10, mid)).done()
	state.bytes(8, put(turn)).varint(10, 1).str(22, "cli")
	rc := (&pb{}).bytes(4, envMessage())
	mcp := &pb{}
	for _, t := range tools {
		d := toolDef(t)
		rc.bytes(7, d)
		mcp.bytes(1, d)
	}
	action := (&pb{}).embed(2, (&pb{}).embed(2, rc))
	details := (&pb{}).str(1, model).str(3, model).str(4, model)
	requested := (&pb{}).str(1, model)
	if maxMode {
		details.varint(7, 1)
		requested.varint(2, 1)
	}
	for _, p := range params {
		requested.embed(3, (&pb{}).str(1, p.ID).str(2, p.Value))
	}
	if conv == "" {
		conv = newID()
	}
	rr := (&pb{}).embed(1, state).embed(2, action).embed(3, details).embed(4, mcp).str(5, conv).embed(9, requested).varint(19, 1)
	return (&pb{}).embed(1, rr).done(), blobs
}

type prepared struct {
	first  []byte
	blobs  map[string][]byte
	convID string
	est    int
	tools  []Tool
}

func (c Chat) Estimate() int { return c.transcript().est }

func prepare(c Chat, resolved Resolved) prepared {
	tr := c.transcript()
	first, blobs := buildRun(tr.order, tr.last, c.Tools, resolved.ID, tr.convID, resolved.MaxMode, resolved.Params)
	return prepared{first: first, blobs: blobs, convID: tr.convID, est: tr.est, tools: c.Tools}
}
