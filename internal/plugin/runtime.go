package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cpa-cursor/internal/cursor"
)

const version = "0.1.1"

type Fail struct {
	Status int
	Code   string
	Msg    string
}

func (f *Fail) Error() string {
	if f == nil {
		return ""
	}
	return f.Msg
}

func fail(status int, code, msg string) *Fail {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if code == "" {
		code = "cursor_error"
	}
	return &Fail{Status: status, Code: code, Msg: msg}
}

func AsFail(err error) *Fail {
	if f := asFail(err); f != nil {
		return f
	}
	return fail(500, "cursor_error", "unknown error")
}

func asFail(err error) *Fail {
	if err == nil {
		return nil
	}
	if f, ok := err.(*Fail); ok {
		return f
	}
	if s, ok := err.(*cursor.StatusError); ok {
		return fail(s.Status, "cursor_error", s.Message)
	}
	return fail(http.StatusBadGateway, "cursor_error", err.Error())
}

type Runtime struct {
	Agent   *cursor.Agent
	host    func(method string, payload any) ([]byte, error)
	pending pendingLogins
}

func New() *Runtime { return &Runtime{Agent: cursor.NewAgent()} }

func (r *Runtime) SetHost(fn func(method string, payload any) ([]byte, error)) {
	if r != nil {
		r.host = fn
	}
}

func Registration() map[string]any {
	return map[string]any{
		"schema_version": 6,
		"metadata": map[string]any{
			"Name":             "Cursor Agent",
			"Version":          version,
			"Author":           "endverse",
			"GitHubRepository": "https://github.com/endverse/cpa-cursor-plugin",
		},
		"capabilities": map[string]any{
			"executor":                true,
			"executor_model_scope":    "both",
			"executor_input_formats":  []string{"claude", "openai-response"},
			"executor_output_formats": []string{"claude", "openai-response"},
			"auth_provider":           true,
			"model_provider":          true,
			"quota_provider":          true,
			"management_api":          true,
		},
	}
}

type execIn struct {
	Model           string
	Format          string
	SourceFormat    string
	Alt             string
	Payload         []byte
	OriginalRequest []byte
	Metadata        map[string]any
	StorageJSON     []byte
	StreamID        string `json:"stream_id"`
}

func (r *Runtime) Call(ctx context.Context, method string, payload []byte) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return Registration(), nil
	case "plugin.quiesce", "plugin.shutdown":
		return map[string]any{}, nil
	case "auth.identifier", "executor.identifier", "quota.identifier":
		return map[string]any{"identifier": "cursor"}, nil
	case "model.static":
		// The alias page prefers this static list and then ignores the account
		// catalog. Leave it empty so that page follows model.for_auth, which CPA
		// refreshes when the credential is loaded or updated.
		return map[string]any{"Provider": "cursor", "Models": []any{}}, nil
	case "auth.parse":
		return r.callParse(payload)
	case "auth.login.start":
		return r.callLoginStart(ctx, payload)
	case "auth.login.poll":
		return r.callLoginPoll(ctx, payload)
	case "management.register":
		return map[string]any{
			"resources": []any{
				map[string]any{
					"Path":        "/login",
					"Menu":        "登录",
					"Description": "打开后跳转到 Cursor 登录，完成后自动保存账号",
				},
				map[string]any{
					"Path":        "/models",
					"Menu":        "模型目录",
					"Description": "给 Codex 的模型菜单返回当前账号里的模型",
				},
			},
		}, nil
	case "management.handle":
		return r.callManagement(ctx, payload)
	case "auth.refresh":
		return r.callRefresh(payload)
	case "model.for_auth":
		return r.callModels(ctx, payload)
	case "executor.count_tokens":
		return r.callCount(payload)
	case "executor.execute":
		body, err := r.callExecute(ctx, payload)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"Payload": body,
			"Headers": http.Header{"Content-Type": {"application/json"}},
		}, nil
	case "quota.describe":
		return map[string]any{"supported_providers": []string{"cursor"}, "display_name": "Cursor"}, nil
	case "quota.fetch":
		return r.callQuota(ctx, payload)
	case "quota.reset":
		return nil, fail(http.StatusNotImplemented, "unsupported", "Cursor quota cannot be reset from this plugin")
	default:
		return nil, fail(http.StatusNotImplemented, "unknown_method", "unknown method: "+method)
	}
}

func (r *Runtime) OpenStream(ctx context.Context, payload []byte) (http.Header, <-chan []byte, error) {
	in, chat, tok, ag, err := r.prepare(ctx, payload)
	if err != nil {
		return nil, nil, err
	}
	if in.Alt == "responses/compact" || strings.Contains(strings.ToLower(asString(in.Metadata["request_path"])), "responses/compact") {
		return nil, nil, fail(http.StatusNotImplemented, "unsupported", "Cursor does not support responses/compact")
	}
	events := ag.Chat(ctx, tok, chat)
	out := make(chan []byte, 8)
	go func() {
		defer close(out)
		_ = Stream(chatFormat(in), displayModel(in, chat), events, func(b []byte) error {
			out <- append([]byte(nil), b...)
			return nil
		})
	}()
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Cursor-Continuity", "stateless; bootstrap=imported-history")
	return h, out, nil
}

func (r *Runtime) callExecute(ctx context.Context, payload []byte) ([]byte, error) {
	in, chat, tok, ag, err := r.prepare(ctx, payload)
	if err != nil {
		return nil, err
	}
	if in.Alt == "responses/compact" {
		return nil, fail(http.StatusNotImplemented, "unsupported", "Cursor does not support responses/compact")
	}
	return Collect(chatFormat(in), displayModel(in, chat), ag.Chat(ctx, tok, chat))
}

func (r *Runtime) prepare(ctx context.Context, payload []byte) (execIn, cursor.Chat, string, *cursor.Agent, error) {
	var in execIn
	if len(payload) > 0 && json.Unmarshal(payload, &in) != nil {
		return in, cursor.Chat{}, "", nil, fail(http.StatusBadRequest, "invalid_request", "executor request is not JSON")
	}
	if in.Alt == "responses/compact" {
		return in, cursor.Chat{}, "", nil, fail(http.StatusNotImplemented, "unsupported", "Cursor does not support responses/compact")
	}
	body := in.Payload
	if len(body) == 0 {
		body = in.OriginalRequest
	}
	format := normFormat(in.Format)
	if format == "" {
		format = normFormat(in.SourceFormat)
	}
	in.Format = format
	chat, err := Parse(format, body, in.Metadata)
	if err != nil {
		return in, cursor.Chat{}, "", nil, fail(http.StatusBadRequest, "invalid_request", err.Error())
	}
	if chat.Model == "" || chat.Model == "auto" {
		if m := strings.TrimPrefix(in.Model, "cursor/"); m != "" {
			chat.Model = m
		}
	}
	tok, ag, err := r.token(ctx, in.StorageJSON, "")
	if err != nil {
		return in, cursor.Chat{}, "", nil, asFail(err)
	}
	return in, chat, tok, ag, nil
}

func chatFormat(in execIn) string {
	if in.Format != "" {
		return in.Format
	}
	return "claude"
}

func displayModel(in execIn, chat cursor.Chat) string {
	if in.Model != "" {
		return in.Model
	}
	return chat.Model
}

func (r *Runtime) callCount(payload []byte) (any, error) {
	var in execIn
	if json.Unmarshal(payload, &in) != nil {
		return nil, fail(http.StatusBadRequest, "invalid_request", "executor request is not JSON")
	}
	body := in.Payload
	if len(body) == 0 {
		body = in.OriginalRequest
	}
	format := normFormat(in.Format)
	if format == "" {
		format = normFormat(in.SourceFormat)
	}
	chat, err := Parse(format, body, in.Metadata)
	if err != nil {
		return nil, fail(http.StatusBadRequest, "invalid_request", err.Error())
	}
	n := chat.Estimate()
	raw, _ := json.Marshal(map[string]int{"input_tokens": n})
	return map[string]any{"Payload": raw, "Headers": http.Header{"Content-Type": {"application/json"}}}, nil
}

func (r *Runtime) callParse(payload []byte) (any, error) {
	var req struct {
		FileName string
		RawJSON  []byte
	}
	if json.Unmarshal(payload, &req) != nil {
		return map[string]any{"Handled": false}, nil
	}
	if res, ok := r.parseAuth(req.RawJSON, req.FileName); ok {
		return res, nil
	}
	return map[string]any{"Handled": false}, nil
}

func (r *Runtime) callLoginStart(ctx context.Context, payload []byte) (any, error) {
	var req struct {
		Metadata map[string]any
		Host     struct{ ProxyURL string }
	}
	_ = json.Unmarshal(payload, &req)
	r.Agent.UseProxy(req.Host.ProxyURL)
	return r.startLogin(ctx, req.Metadata)
}

func (r *Runtime) callLoginPoll(ctx context.Context, payload []byte) (any, error) {
	var req struct {
		State    string
		Metadata map[string]any
		Host     struct{ ProxyURL string }
	}
	if json.Unmarshal(payload, &req) != nil || req.State == "" {
		return map[string]any{"Status": "error", "Message": "login state is missing"}, nil
	}
	r.Agent.UseProxy(req.Host.ProxyURL)
	return r.pollLogin(ctx, req.State, req.Metadata)
}

func (r *Runtime) callManagement(ctx context.Context, payload []byte) (any, error) {
	var req struct {
		Method string
		Path   string
	}
	_ = json.Unmarshal(payload, &req)
	path := req.Path
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimRight(path, "/")
	if !strings.EqualFold(req.Method, http.MethodGet) {
		return map[string]any{"StatusCode": http.StatusNotFound, "Body": []byte("not found")}, nil
	}
	if strings.HasSuffix(path, "/models") {
		return r.serveCodexCatalog()
	}
	if !strings.HasSuffix(path, "/login") {
		return map[string]any{"StatusCode": http.StatusNotFound, "Body": []byte("not found")}, nil
	}
	started, err := r.startLogin(ctx, nil)
	if err != nil {
		return map[string]any{
			"StatusCode": http.StatusBadGateway,
			"Headers":    http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
			"Body":       []byte("Cursor 登录没有打开。"),
		}, nil
	}
	loginURL := asString(started["URL"])
	if loginURL == "" {
		return map[string]any{
			"StatusCode": http.StatusBadGateway,
			"Headers":    http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
			"Body":       []byte("Cursor 登录没有打开。"),
		}, nil
	}
	go r.watchLogin(asString(started["State"]))
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": http.Header{
			"Content-Type":  {"text/html; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": loginPage(loginURL),
	}, nil
}

func loginPage(loginURL string) []byte {
	return []byte("<!doctype html><meta charset=\"utf-8\"><title>Cursor 登录</title><script>window.top.location.replace(" + strconv.Quote(loginURL) + ")</script>")
}

func (r *Runtime) watchLogin(state string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var done map[string]any
	for {
		if done == nil {
			res, err := r.pollLogin(ctx, state, nil)
			if err == nil {
				switch res["Status"] {
				case "success":
					done = res
				case "error":
					return
				}
			}
		}
		if done != nil && r.saveLogin(done) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(loginPollEvery):
		}
	}
}

func (r *Runtime) saveLogin(res map[string]any) bool {
	if r == nil || r.host == nil {
		return false
	}
	auth, _ := res["Auth"].(map[string]any)
	name, _ := auth["FileName"].(string)
	raw, _ := auth["StorageJSON"].([]byte)
	if name == "" || len(raw) == 0 {
		return false
	}
	_, err := r.host("host.auth.save", map[string]any{"name": name, "json": json.RawMessage(raw)})
	return err == nil
}

func (r *Runtime) callRefresh(payload []byte) (any, error) {
	var req struct {
		StorageJSON []byte
		AuthID      string
	}
	_ = json.Unmarshal(payload, &req)
	doc, ok := parseStored(req.StorageJSON)
	if !ok {
		return nil, fail(http.StatusUnauthorized, "cursor_auth", "Cursor isn't signed in")
	}
	if doc.CursorAuth.Type == "oauth" || doc.CursorAuth.Type == "" {
		exp := doc.CursorAuth.Expires
		if exp.IsZero() {
			exp = cursor.JWTExpiry(doc.CursorAuth.Access)
		}
		if !exp.IsZero() && !exp.After(time.Now()) {
			return nil, fail(http.StatusUnauthorized, "cursor_auth", "Cursor's sign-in has run out; sign in to Cursor again")
		}
	}
	id := req.AuthID
	if id == "" {
		id = doc.CursorAuth.Email
	}
	return map[string]any{
		"Auth": map[string]any{
			"Provider": "cursor", "ID": id, "StorageJSON": req.StorageJSON,
		},
		"NextRefreshAfter": time.Now().Add(time.Hour),
	}, nil
}

func (r *Runtime) callModels(ctx context.Context, payload []byte) (any, error) {
	var req struct {
		StorageJSON []byte
		Host        struct{ ProxyURL string }
	}
	_ = json.Unmarshal(payload, &req)
	tok, ag, err := r.token(ctx, req.StorageJSON, req.Host.ProxyURL)
	if err != nil {
		return nil, asFail(err)
	}
	entries, err := ag.Models(ctx, tok)
	if err != nil {
		return nil, asFail(err)
	}
	return map[string]any{"Provider": "cursor", "Models": modelList(entries)}, nil
}

func modelList(entries []cursor.Entry) []any {
	models := make([]any, 0, len(entries))
	for _, e := range entries {
		var names []string
		for _, l := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
			if l == "none" || e.Levels[l] != "" {
				names = append(names, l)
			}
		}
		models = append(models, map[string]any{
			"ID": e.ID, "Object": "model", "OwnedBy": "cursor", "Type": "cursor",
			"DisplayName": cursorDisplayName(e), "ContextLength": e.Context,
			"SupportedInputModalities":  []string{"text", "image"},
			"SupportedOutputModalities": []string{"text"},
			"Thinking":                  map[string]any{"ZeroAllowed": true, "DynamicAllowed": true, "Levels": names},
		})
	}
	return models
}

func cursorDisplayName(e cursor.Entry) string {
	title := strings.TrimSpace(e.Title)
	if title == "" {
		title = strings.TrimSpace(e.ID)
	}
	if title == "" {
		return "Cursor"
	}
	name := "Cursor · " + title
	if size := strings.TrimSpace(e.Size); size != "" {
		name += " · " + size
	}
	return name
}

func autoModel() map[string]any {
	return map[string]any{
		"ID": "auto", "Object": "model", "OwnedBy": "cursor", "Type": "cursor",
		"DisplayName": "Cursor · Auto", "ContextLength": 200000,
		"Thinking": map[string]any{"ZeroAllowed": true, "DynamicAllowed": true, "Levels": []string{"none", "low", "medium", "high", "xhigh", "max"}},
	}
}

func (r *Runtime) callQuota(ctx context.Context, payload []byte) (any, error) {
	var req struct {
		StorageJSON []byte `json:"storage_json"`
		Host        struct {
			ProxyURL string
		} `json:"host"`
	}
	_ = json.Unmarshal(payload, &req)
	tok, ag, err := r.token(ctx, req.StorageJSON, req.Host.ProxyURL)
	if err != nil {
		return nil, asFail(err)
	}
	plan, windows, err := ag.Usage(ctx, tok)
	if err != nil {
		return nil, asFail(err)
	}
	var groups []any
	for _, w := range windows {
		remain := 1 - w.Used/100
		if remain < 0 {
			remain = 0
		}
		if remain > 1 {
			remain = 1
		}
		groups = append(groups, map[string]any{
			"displayName": w.Name,
			"buckets": []any{map[string]any{
				"window": w.Name, "remainingFraction": remain, "resetTime": w.ResetsAt,
			}},
		})
	}
	out := map[string]any{"groups": groups}
	if plan != "" {
		out["subscription"] = map[string]any{"plan": plan}
	}
	return out, nil
}
