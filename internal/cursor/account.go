package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	defaultAPI       = "https://api2.cursor.sh"
	defaultAgentHost = "https://agentn.global.api5.cursor.sh"
	versionFallback  = "cli-2026.09.23-86fc751"
)

type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func statusErr(status int, msg string) *StatusError {
	if status == 0 {
		status = http.StatusBadGateway
	}
	return &StatusError{Status: status, Message: msg}
}

func failure(status int, text string) *StatusError {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Debug struct {
					Details struct {
						Title  string `json:"title"`
						Detail string `json:"detail"`
					} `json:"details"`
				} `json:"debug"`
			} `json:"details"`
		} `json:"error"`
		Details []struct {
			Debug struct {
				Details struct {
					Title  string `json:"title"`
					Detail string `json:"detail"`
				} `json:"details"`
			} `json:"debug"`
		} `json:"details"`
	}
	if json.Unmarshal([]byte(text), &e) != nil {
		return statusOf(status, "", strings.TrimSpace(text))
	}
	code := e.Code
	msg := e.Message
	details := e.Details
	if e.Error != nil {
		if e.Error.Code != "" {
			code = e.Error.Code
		}
		if e.Error.Message != "" {
			msg = e.Error.Message
		}
		if len(e.Error.Details) > 0 {
			details = e.Error.Details
		}
	}
	for _, d := range details {
		t := strings.TrimSpace(d.Debug.Details.Title + ": " + d.Debug.Details.Detail)
		t = strings.Trim(t, ": ")
		if t != "" {
			msg = t
		}
	}
	if msg == "" || msg == "Error" {
		msg = code
	}
	if code == "" && status >= 200 && status < 300 {
		return statusErr(status, msg)
	}
	return statusOf(status, code, msg)
}

func statusOf(status int, code, msg string) *StatusError {
	if msg == "" {
		msg = http.StatusText(status)
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "region"):
		return statusErr(http.StatusForbidden, msg+" — Cursor serves this team only in some regions")
	case code == "permission_denied":
		return statusErr(http.StatusForbidden, msg)
	case code == "unauthenticated" || status == http.StatusUnauthorized || strings.Contains(low, "expired"):
		return statusErr(http.StatusUnauthorized, msg+" — sign in to Cursor again")
	case code == "resource_exhausted" || strings.Contains(low, "quota") || strings.Contains(low, "rate limit") || strings.Contains(low, "usage limit"):
		return statusErr(http.StatusTooManyRequests, "usage limit reached: "+msg)
	case strings.Contains(low, "too long") || strings.Contains(low, "context length") || strings.Contains(low, "too many tokens"):
		return statusErr(http.StatusBadRequest, "input is too long for the model's context: "+msg)
	case code == "invalid_argument":
		return statusErr(http.StatusBadRequest, msg)
	case code == "unavailable":
		return statusErr(http.StatusServiceUnavailable, msg)
	case code != "":
		return statusErr(http.StatusBadGateway, msg)
	default:
		if status == 0 {
			status = http.StatusBadGateway
		}
		return statusErr(status, msg)
	}
}

func maxRequired(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "max mode required") || strings.Contains(low, "max_mode_required") || strings.Contains(low, "requires max mode")
}

func regional(msg string) bool { return strings.Contains(strings.ToLower(msg), "region") }

type endpoint struct {
	url    string
	at     time.Time
	listed bool
}

type Agent struct {
	API          string
	AgentHost    string
	Client       *http.Client
	H2           *http.Client
	ForceH1      bool
	Version      string
	mu           sync.Mutex
	h1Until      time.Time
	endpoints    map[string]endpoint
	catalogs     map[string]catalog
	exchanged    map[string]exchangedToken
	team         teamCache
	versionOnce  sync.Once
	versionValue string
}

type catalog struct {
	at      time.Time
	entries []Entry
}

type exchangedToken struct {
	tok string
	exp time.Time
}

type teamCache struct {
	at   time.Time
	info *teamInfo
}

type teamInfo struct {
	AuthID       string
	ActiveTeamID int64
}

func NewAgent() *Agent {
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, DisableCompression: true}
	h2 := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, DisableCompression: true}
	return &Agent{
		API:       defaultAPI,
		AgentHost: defaultAgentHost,
		Client:    &http.Client{Transport: tr},
		H2:        &http.Client{Transport: h2},
		endpoints: map[string]endpoint{},
		catalogs:  map[string]catalog{},
		exchanged: map[string]exchangedToken{},
	}
}

func (a *Agent) Headers(tok, contentType string) http.Header {
	return a.headers(tok, contentType)
}

// JWTExpiry is when a Cursor access token runs out.
func JWTExpiry(tok string) time.Time { return jwtExpiry(tok) }

// UseProxy points later requests through proxyURL. An empty URL keeps the
// current client. Two credentials with different proxies share this client;
// the most recent one wins.
func (a *Agent) UseProxy(proxyURL string) *Agent {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" || a == nil {
		return a
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return a
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u), ForceAttemptHTTP2: true, DisableCompression: true}
	h2 := &http.Transport{Proxy: http.ProxyURL(u), ForceAttemptHTTP2: true, DisableCompression: true}
	a.mu.Lock()
	a.Client = &http.Client{Transport: tr}
	a.H2 = &http.Client{Transport: h2}
	a.mu.Unlock()
	return a
}

func (a *Agent) clientVersion() string {
	if a.Version != "" {
		return a.Version
	}
	a.versionOnce.Do(func() {
		a.versionValue = detectVersion()
	})
	if a.versionValue == "" {
		return versionFallback
	}
	return a.versionValue
}

func detectVersion() string {
	home, _ := os.UserHomeDir()
	dirs := []string{filepath.Join(home, ".local", "share", "cursor-agent", "versions")}
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			dirs = append(dirs, filepath.Join(local, "cursor-agent", "versions"))
		}
	}
	best := ""
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if versionRE(name) && name > best {
				best = name
			}
		}
	}
	if best == "" {
		return versionFallback
	}
	return "cli-" + best
}

func versionRE(s string) bool {
	// YYYY.MM.DD-hex
	if len(s) < 12 || s[4] != '.' || s[7] != '.' || !strings.Contains(s, "-") {
		return false
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 || parts[1] == "" {
		return false
	}
	for _, c := range parts[1] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (a *Agent) headers(tok string, contentType string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", contentType)
	h.Set("Connect-Protocol-Version", "1")
	h.Set("x-cursor-client-version", a.clientVersion())
	h.Set("x-cursor-client-type", "cli")
	h.Set("x-ghost-mode", "true")
	if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	if id := a.teamID(tok); id != "" {
		h.Set("x-cursor-team-id", id)
	}
	return h
}

func (a *Agent) teamID(tok string) string {
	info := a.readTeam()
	if info == nil || info.AuthID == "" || info.AuthID != jwtSub(tok) || info.ActiveTeamID <= 0 {
		return ""
	}
	return strconvFormat(info.ActiveTeamID)
}

func strconvFormat(n int64) string {
	return strings.TrimPrefix(strings.Replace(jsonNumber(n), "", "", 1), "")
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (a *Agent) readTeam() *teamInfo {
	a.mu.Lock()
	if time.Since(a.team.at) < 10*time.Second {
		info := a.team.info
		a.mu.Unlock()
		return info
	}
	a.mu.Unlock()
	info := loadTeam()
	a.mu.Lock()
	a.team = teamCache{at: time.Now(), info: info}
	a.mu.Unlock()
	return info
}

func loadTeam() *teamInfo {
	home, _ := os.UserHomeDir()
	dir := strings.TrimSpace(os.Getenv("CURSOR_CONFIG_DIR"))
	if dir == "" {
		if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
			dir = filepath.Join(xdg, "cursor")
		} else {
			dir = filepath.Join(home, ".cursor")
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "cli-config.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		AuthInfo struct {
			AuthID       string `json:"authId"`
			ActiveTeamID int64  `json:"activeTeamId"`
		} `json:"authInfo"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return &teamInfo{AuthID: doc.AuthInfo.AuthID, ActiveTeamID: doc.AuthInfo.ActiveTeamID}
}

func jwtSub(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Sub
}

func jwtExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func (a *Agent) Unary(ctx context.Context, tok, path string, body any, timeout time.Duration) (map[string]any, error) {
	if body == nil {
		body = map[string]any{}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.API, "/")+"/"+strings.TrimLeft(path, "/"), strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	req.Header = a.headers(tok, "application/json")
	res, err := a.Client.Do(req)
	if err != nil {
		return nil, statusErr(http.StatusBadGateway, "couldn't reach Cursor's API: "+err.Error())
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, failure(res.StatusCode, string(buf))
	}
	if len(bytesTrim(buf)) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if json.Unmarshal(buf, &out) != nil {
		return nil, statusErr(http.StatusBadGateway, "Cursor returned a response that is not JSON")
	}
	return out, nil
}

func bytesTrim(b []byte) []byte { return []byte(strings.TrimSpace(string(b))) }

func (a *Agent) Exchange(ctx context.Context, key string) (string, error) {
	sum := sha256.Sum256([]byte(key))
	id := hexEncode(sum[:])
	a.mu.Lock()
	if had, ok := a.exchanged[id]; ok && (had.exp.IsZero() || time.Until(had.exp) > 5*time.Minute) {
		a.mu.Unlock()
		return had.tok, nil
	}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.API, "/")+"/auth/exchange_user_api_key", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	h := a.headers(key, "application/json")
	h.Del("Connect-Protocol-Version")
	req.Header = h
	res, err := a.Client.Do(req)
	if err != nil {
		return "", statusErr(http.StatusBadGateway, "couldn't reach Cursor's API: "+err.Error())
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 500 {
		return "", statusErr(http.StatusBadGateway, "exchange_user_api_key: HTTP "+jsonNumber(int64(res.StatusCode)))
	}
	var doc struct {
		AccessToken string `json:"accessToken"`
	}
	_ = json.Unmarshal(buf, &doc)
	if doc.AccessToken == "" {
		return "", statusErr(http.StatusUnauthorized, "Cursor didn't take this API key")
	}
	a.mu.Lock()
	a.exchanged[id] = exchangedToken{tok: doc.AccessToken, exp: jwtExpiry(doc.AccessToken)}
	a.mu.Unlock()
	return doc.AccessToken, nil
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0x0f]
	}
	return string(out)
}

func (a *Agent) agentURL(ctx context.Context, tok string, fresh bool) string {
	key := tokenKey(tok)
	a.mu.Lock()
	had, ok := a.endpoints[key]
	a.mu.Unlock()
	if !fresh && ok && (had.listed || time.Since(had.at) < time.Minute) {
		return had.url
	}
	url := a.AgentHost
	listed := false
	cfg, err := a.Unary(ctx, tok, "aiserver.v1.ServerConfigService/GetServerConfig", map[string]any{}, 10*time.Second)
	if err == nil {
		if raw, _ := cfg["agentUrlConfig"].(map[string]any); raw != nil {
			for _, k := range []string{"agentUrl", "agentnUrl"} {
				s, _ := raw[k].(string)
				if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
					url = strings.TrimRight(s, "/")
					listed = true
					break
				}
			}
		}
	}
	a.mu.Lock()
	a.endpoints[key] = endpoint{url: url, at: time.Now(), listed: listed}
	a.mu.Unlock()
	return url
}

func (a *Agent) Models(ctx context.Context, tok string) ([]Entry, error) {
	key := tokenKey(tok)
	a.mu.Lock()
	if had, ok := a.catalogs[key]; ok && time.Since(had.at) < modelsKeep {
		entries := had.entries
		a.mu.Unlock()
		return entries, nil
	}
	a.mu.Unlock()
	type result struct {
		doc map[string]any
		err error
	}
	pickerCh := make(chan result, 1)
	idsCh := make(chan map[string]bool, 1)
	go func() {
		doc, err := a.Unary(ctx, tok, "aiserver.v1.AiService/AvailableModels", map[string]any{"useModelParameters": true, "doNotUseMarkdown": true}, 30*time.Second)
		pickerCh <- result{doc, err}
	}()
	go func() {
		doc, err := a.Unary(ctx, tok, "agent.v1.AgentService/GetUsableModels", map[string]any{}, 8*time.Second)
		if err != nil {
			idsCh <- nil
			return
		}
		ids := map[string]bool{}
		models, _ := doc["models"].([]any)
		for _, m := range models {
			obj, _ := m.(map[string]any)
			if obj == nil {
				continue
			}
			if s, _ := obj["modelId"].(string); s != "" {
				ids[s] = true
			}
			if s, _ := obj["displayModelId"].(string); s != "" {
				ids[s] = true
			}
		}
		if len(ids) == 0 {
			idsCh <- nil
			return
		}
		idsCh <- ids
	}()
	picker := <-pickerCh
	ids := <-idsCh
	if picker.err != nil {
		return nil, picker.err
	}
	rawModels, _ := picker.doc["models"].([]any)
	buf, _ := json.Marshal(rawModels)
	var models []pickerModel
	if json.Unmarshal(buf, &models) != nil {
		return nil, statusErr(http.StatusBadGateway, "Cursor's model list could not be read")
	}
	entries := modelsOf(models, ids)
	if len(entries) == 0 {
		return nil, statusErr(http.StatusBadGateway, "Cursor listed no models")
	}
	a.mu.Lock()
	a.catalogs[key] = catalog{at: time.Now(), entries: entries}
	a.mu.Unlock()
	return entries, nil
}

func (a *Agent) Resolve(ctx context.Context, tok, model, effort string, fast bool) Resolved {
	entries, err := a.Models(ctx, tok)
	if err != nil || len(entries) == 0 {
		return a.resolve(nil, model, effort, fast)
	}
	return a.resolve(entries, model, effort, fast)
}

type UsageWindow struct {
	Name     string
	Used     float64
	ResetsAt string
}

func (a *Agent) Usage(ctx context.Context, tok string) (plan string, windows []UsageWindow, err error) {
	doc, err := a.Unary(ctx, tok, "aiserver.v1.DashboardService/GetCurrentPeriodUsage", map[string]any{}, 15*time.Second)
	if err != nil {
		return "", nil, err
	}
	planDoc, planErr := a.Unary(ctx, tok, "aiserver.v1.DashboardService/GetPlanInfo", map[string]any{}, 15*time.Second)
	if planErr == nil {
		if info, _ := planDoc["planInfo"].(map[string]any); info != nil {
			plan, _ = info["planName"].(string)
		}
	}
	u, _ := doc["planUsage"].(map[string]any)
	if u == nil {
		return plan, nil, nil
	}
	resets := ""
	if s, _ := doc["billingCycleEnd"].(string); s != "" {
		if n, e := json.Number(s).Int64(); e == nil && n > 0 {
			resets = time.UnixMilli(n).UTC().Format(time.RFC3339)
		}
	}
	num := func(k string) float64 {
		switch v := u[k].(type) {
		case float64:
			return v
		default:
			return 0
		}
	}
	windows = []UsageWindow{
		{Name: "Cursor Models", Used: num("autoPercentUsed"), ResetsAt: resets},
		{Name: "Other Models", Used: num("apiPercentUsed"), ResetsAt: resets},
		{Name: "Total", Used: num("totalPercentUsed"), ResetsAt: resets},
	}
	return plan, windows, nil
}

func (a *Agent) Who(ctx context.Context, tok string) (email, plan string) {
	me, err := a.Unary(ctx, tok, "aiserver.v1.DashboardService/GetMe", map[string]any{}, 15*time.Second)
	if err == nil {
		email, _ = me["email"].(string)
		email = strings.TrimSpace(email)
	}
	info, err := a.Unary(ctx, tok, "aiserver.v1.DashboardService/GetPlanInfo", map[string]any{}, 15*time.Second)
	if err == nil {
		if p, _ := info["planInfo"].(map[string]any); p != nil {
			plan, _ = p["planName"].(string)
			plan = strings.TrimSpace(plan)
		}
	}
	return email, plan
}

func ReadCLIToken() (string, error) {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", "cursor-access-token", "-a", "cursor-user", "-w")
		out, err := cmd.Output()
		if err == nil {
			tok := strings.TrimSpace(string(out))
			if tok != "" {
				return tok, nil
			}
		}
	}
	path := cliAuthPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", statusErr(http.StatusUnauthorized, "Cursor isn't signed in; run cursor-agent login or add an auth file")
	}
	var doc struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.AccessToken == "" {
		return "", statusErr(http.StatusUnauthorized, "Cursor isn't signed in; run cursor-agent login or add an auth file")
	}
	return doc.AccessToken, nil
}

func cliAuthPath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("APPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(base, "Cursor", "auth.json")
	case "darwin":
		return filepath.Join(home, ".cursor", "auth.json")
	default:
		if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
			return filepath.Join(xdg, "cursor", "auth.json")
		}
		return filepath.Join(home, ".config", "cursor", "auth.json")
	}
}
