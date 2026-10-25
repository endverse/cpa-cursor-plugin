package plugin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Codex's model picker does not read an OpenAI {data:[{id}]} list. With
// model_catalog_url set, it expects this ModelsResponse and rejects bodies
// over 1 MiB, so the instructions stay short and are not copied per field
// from Codex's own prompt.
const codexInstructions = "You are a coding agent in the user's workspace. Use the tools you are given to inspect the code and make the smallest change that satisfies the request. Do not claim a tool ran unless you actually called it."

func codexCatalog(ids []string) map[string]any {
	seen := map[string]bool{}
	ordered := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ordered = append(ordered, id)
	}
	if i := indexOf(ordered, "auto"); i > 0 {
		ordered = append([]string{"auto"}, append(ordered[:i], ordered[i+1:]...)...)
	}
	models := make([]any, 0, len(ordered))
	for n, id := range ordered {
		window := contextTokens(id)
		models = append(models, map[string]any{
			"slug":                    id,
			"display_name":            id,
			"description":             "Cursor",
			"default_reasoning_level": "low",
			"supported_reasoning_levels": []any{
				map[string]string{"effort": "low", "description": "Faster, lighter reasoning"},
				map[string]string{"effort": "medium", "description": "Balanced reasoning"},
				map[string]string{"effort": "high", "description": "Deeper reasoning"},
				map[string]string{"effort": "xhigh", "description": "Extra reasoning"},
			},
			"shell_type":                       "unified_exec",
			"visibility":                       "list",
			"supported_in_api":                 true,
			"priority":                         n + 1,
			"upgrade":                          nil,
			"support_verbosity":                false,
			"default_verbosity":                nil,
			"apply_patch_tool_type":            "freeform",
			"truncation_policy":                map[string]any{"mode": "tokens", "limit": 10000},
			"context_window":                   window,
			"max_context_window":               window,
			"effective_context_window_percent": 95,
			"experimental_supported_tools":     []any{},
			"input_modalities":                 []string{"text", "image"},
			"supports_search_tool":             false,
			"use_responses_lite":               false,
			"base_instructions":                codexInstructions,
			"default_reasoning_summary":        "none",
		})
	}
	return map[string]any{"models": models}
}

func contextTokens(id string) int64 {
	const fallback int64 = 200_000
	if !strings.Contains(id, "@") {
		return fallback
	}
	size := strings.ToLower(strings.TrimSpace(id[strings.LastIndex(id, "@")+1:]))
	mult := int64(1)
	switch {
	case strings.HasSuffix(size, "k"):
		mult, size = 1000, strings.TrimSuffix(size, "k")
	case strings.HasSuffix(size, "m"):
		mult, size = 1_000_000, strings.TrimSuffix(size, "m")
	}
	n, err := strconv.ParseFloat(size, 64)
	if err != nil || n <= 0 {
		return fallback
	}
	return int64(n * float64(mult))
}

func indexOf(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}

func (r *Runtime) serveCodexCatalog() (any, error) {
	ids, err := r.codexModelIDs()
	if err != nil {
		status := http.StatusBadGateway
		message := err.Error()
		if f, ok := err.(*Fail); ok {
			status = f.Status
			message = f.Msg
		}
		return map[string]any{
			"StatusCode": status,
			"Headers":    http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
			"Body":       []byte(message),
		}, nil
	}
	body, err := json.Marshal(codexCatalog(ids))
	if err != nil {
		return nil, fail(http.StatusInternalServerError, "cursor_error", err.Error())
	}
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": http.Header{
			"Content-Type":  {"application/json"},
			"Cache-Control": {"no-store"},
		},
		"Body": body,
	}, nil
}

func (r *Runtime) codexModelIDs() ([]string, error) {
	if r == nil || r.host == nil {
		return nil, fail(http.StatusServiceUnavailable, "no_models", "Cursor model list is not available yet")
	}
	raw, err := r.host("host.auth.models", map[string]any{})
	if err != nil {
		return nil, fail(http.StatusBadGateway, "cursor_error", err.Error())
	}
	result, err := decodeHostResult(raw)
	if err != nil {
		return nil, fail(http.StatusBadGateway, "cursor_error", err.Error())
	}
	var listed struct {
		Credentials []struct {
			Provider string   `json:"provider"`
			Models   []string `json:"models"`
		} `json:"credentials"`
	}
	if json.Unmarshal(result, &listed) != nil {
		return nil, fail(http.StatusBadGateway, "cursor_error", "model list from CPA could not be read")
	}
	var cursorIDs, other []string
	for _, cred := range listed.Credentials {
		dst := &other
		if strings.EqualFold(cred.Provider, "cursor") {
			dst = &cursorIDs
		}
		*dst = append(*dst, cred.Models...)
	}
	if len(cursorIDs) > 0 {
		return cursorIDs, nil
	}
	if len(other) > 0 {
		return other, nil
	}
	return nil, fail(http.StatusServiceUnavailable, "no_models", "Cursor model list is not available yet")
}

func decodeHostResult(raw []byte) (json.RawMessage, error) {
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		message := "CPA rejected the model list request"
		if env.Error != nil && env.Error.Message != "" {
			message = env.Error.Message
		}
		return nil, fail(http.StatusBadGateway, "cursor_error", message)
	}
	if len(env.Result) == 0 {
		env.Result = []byte(`{}`)
	}
	return env.Result, nil
}
