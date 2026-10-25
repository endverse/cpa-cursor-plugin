package plugin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"cpa-cursor/internal/cursor"
)

// loginPollEvery is how often a menu-page login waits for the browser.
// CPA's own status poll is separate and calls auth.login.poll itself.
var loginPollEvery = 2 * time.Second

type credential struct {
	Type    string    `json:"type"`
	Key     string    `json:"key,omitempty"`
	Access  string    `json:"access,omitempty"`
	Refresh string    `json:"refresh,omitempty"`
	Expires time.Time `json:"expires,omitempty"`
	Email   string    `json:"email,omitempty"`
}

type storedAuth struct {
	Type       string     `json:"type"`
	CursorAuth credential `json:"cursor_auth"`
	ProxyURL   string     `json:"proxy_url,omitempty"`
}

func parseStored(raw []byte) (storedAuth, bool) {
	var doc storedAuth
	if json.Unmarshal(raw, &doc) != nil {
		return storedAuth{}, false
	}
	if doc.Type != "cursor" && doc.CursorAuth.Type == "" && doc.CursorAuth.Access == "" && doc.CursorAuth.Key == "" {
		return storedAuth{}, false
	}
	if doc.Type == "" {
		doc.Type = "cursor"
	}
	return doc, doc.Type == "cursor"
}

func (r *Runtime) token(ctx context.Context, raw []byte, hostProxy string) (string, *cursor.Agent, error) {
	doc, ok := parseStored(raw)
	if !ok {
		return "", r.Agent, fail(http.StatusUnauthorized, "cursor_auth", "Cursor isn't signed in")
	}
	ag := r.Agent.UseProxy(firstNonEmpty(doc.ProxyURL, hostProxy))
	switch doc.CursorAuth.Type {
	case "api":
		if doc.CursorAuth.Key == "" {
			return "", ag, fail(http.StatusUnauthorized, "cursor_auth", "Cursor API key is missing")
		}
		tok, err := ag.Exchange(ctx, doc.CursorAuth.Key)
		return tok, ag, err
	case "cli":
		tok, err := cliAccess()
		return tok, ag, err
	default:
		if doc.CursorAuth.Access == "" {
			return "", ag, fail(http.StatusUnauthorized, "cursor_auth", "Cursor isn't signed in")
		}
		exp := doc.CursorAuth.Expires
		if exp.IsZero() {
			exp = cursor.JWTExpiry(doc.CursorAuth.Access)
		}
		if !exp.IsZero() && !exp.After(time.Now()) {
			return "", ag, fail(http.StatusUnauthorized, "cursor_auth", "Cursor's sign-in has run out; sign in to Cursor again")
		}
		return doc.CursorAuth.Access, ag, nil
	}
}

func cliAccess() (string, error) {
	tok, err := cursor.ReadCLIToken()
	if err != nil {
		return "", err
	}
	exp := cursor.JWTExpiry(tok)
	if !exp.IsZero() && time.Until(exp) < 5*time.Minute {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, name := range []string{"cursor-agent", "agent"} {
			cmd := exec.CommandContext(ctx, name, "status")
			if cmd.Run() == nil {
				break
			}
		}
		if fresh, err2 := cursor.ReadCLIToken(); err2 == nil && fresh != "" {
			tok = fresh
		}
	}
	exp = cursor.JWTExpiry(tok)
	if !exp.IsZero() && !exp.After(time.Now()) {
		return "", fail(http.StatusUnauthorized, "cursor_auth", "Cursor's sign-in has run out; sign in to Cursor again")
	}
	return tok, nil
}

func authRecord(access, refresh, email string, expires time.Time) map[string]any {
	cred := credential{Type: "oauth", Access: access, Refresh: refresh, Expires: expires, Email: email}
	if refresh == "" {
		cred.Refresh = "none"
	}
	raw, _ := json.Marshal(storedAuth{Type: "cursor", CursorAuth: cred})
	id := email
	if id == "" {
		sum := sha256.Sum256([]byte(access))
		id = "cursor-" + hex.EncodeToString(sum[:4])
	}
	label := email
	if label == "" {
		label = "Cursor"
	}
	return map[string]any{
		"Provider":    "cursor",
		"ID":          id,
		"Label":       label,
		"FileName":    "cursor-" + id + ".json",
		"StorageJSON": raw,
	}
}

type loginState struct {
	UUID     string
	Verifier string
	Exp      int64
	APIKey   string
}

type pendingLogins struct {
	mu sync.Mutex
	m  map[string]loginState
}

func (p *pendingLogins) put(id string, st loginState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]loginState{}
	}
	now := time.Now().Unix()
	for k, v := range p.m {
		if v.Exp < now {
			delete(p.m, k)
		}
	}
	p.m[id] = st
}

func (p *pendingLogins) get(id string) (loginState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.m[id]
	if !ok || st.Exp < time.Now().Unix() {
		return loginState{}, false
	}
	return st, true
}

// shortState is the token CPA stores and polls. CPA rejects anything longer
// than 128 characters or outside [A-Za-z0-9._-], so the PKCE verifier stays
// in memory and in Metadata, not in this string.
func shortState() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (r *Runtime) startLogin(_ context.Context, meta map[string]any) (map[string]any, error) {
	exp := time.Now().Add(10 * time.Minute)
	state, err := shortState()
	if err != nil {
		return nil, err
	}
	if key := firstNonEmpty(asString(meta["api_key"]), asString(meta["cursor_api_key"])); key != "" {
		r.pending.put(state, loginState{APIKey: key, Exp: exp.Unix()})
		return map[string]any{
			"Provider":  "cursor",
			"URL":       "",
			"State":     state,
			"ExpiresAt": exp,
		}, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	id := randID("")
	r.pending.put(state, loginState{UUID: id, Verifier: verifier, Exp: exp.Unix()})
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("uuid", id)
	q.Set("mode", "login")
	q.Set("redirectTarget", "cli")
	return map[string]any{
		"Provider":  "cursor",
		"URL":       "https://cursor.com/loginDeepControl?" + q.Encode(),
		"State":     state,
		"ExpiresAt": exp,
		"Metadata":  map[string]any{"uuid": id, "verifier": verifier},
	}, nil
}

func (r *Runtime) pollLogin(ctx context.Context, state string, meta map[string]any) (map[string]any, error) {
	st, ok := r.pending.get(state)
	if !ok {
		st = loginState{UUID: asString(meta["uuid"]), Verifier: asString(meta["verifier"]), Exp: time.Now().Add(time.Minute).Unix()}
	}
	if st.APIKey == "" && (st.UUID == "" || st.Verifier == "") {
		return map[string]any{"Status": "error", "Message": "Cursor sign-in expired; start it again"}, nil
	}
	if st.APIKey != "" {
		tok, err := r.Agent.Exchange(ctx, st.APIKey)
		if err != nil {
			return map[string]any{"Status": "error", "Message": err.Error()}, nil
		}
		email, _ := r.Agent.Who(ctx, tok)
		raw, _ := json.Marshal(storedAuth{Type: "cursor", CursorAuth: credential{Type: "api", Key: st.APIKey, Email: email}})
		id := email
		if id == "" {
			sum := sha256.Sum256([]byte(st.APIKey))
			id = "cursor-" + hex.EncodeToString(sum[:4])
		}
		return map[string]any{
			"Status": "success",
			"Auth": map[string]any{
				"Provider": "cursor", "ID": id, "Label": firstNonEmpty(email, "Cursor"),
				"FileName": "cursor-" + id + ".json", "StorageJSON": raw,
			},
		}, nil
	}
	u := strings.TrimRight(r.Agent.API, "/") + "/auth/poll?uuid=" + url.QueryEscape(st.UUID) + "&verifier=" + url.QueryEscape(st.Verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header = r.Agent.Headers("", "application/json")
	res, err := r.Agent.Client.Do(req)
	if err != nil {
		return map[string]any{"Status": "pending", "Message": "waiting for the browser sign-in"}, nil
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusNotFound {
		return map[string]any{"Status": "pending", "Message": "waiting for the browser sign-in"}, nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return map[string]any{"Status": "error", "Message": fmt.Sprintf("Cursor's sign-in answered HTTP %d", res.StatusCode)}, nil
	}
	var doc struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if json.Unmarshal(buf, &doc) != nil || doc.AccessToken == "" {
		return map[string]any{"Status": "pending", "Message": "waiting for the browser sign-in"}, nil
	}
	email, _ := r.Agent.Who(ctx, doc.AccessToken)
	exp := cursor.JWTExpiry(doc.AccessToken)
	if exp.IsZero() {
		exp = time.Now().Add(30 * 24 * time.Hour)
	}
	return map[string]any{"Status": "success", "Auth": authRecord(doc.AccessToken, doc.RefreshToken, email, exp)}, nil
}

func (r *Runtime) parseAuth(raw []byte, file string) (map[string]any, bool) {
	doc, ok := parseStored(raw)
	if !ok {
		return nil, false
	}
	id := doc.CursorAuth.Email
	if id == "" {
		secret := firstNonEmpty(doc.CursorAuth.Access, doc.CursorAuth.Key, file)
		sum := sha256.Sum256([]byte(secret))
		id = "cursor-" + hex.EncodeToString(sum[:4])
	}
	label := doc.CursorAuth.Email
	if label == "" {
		label = "Cursor"
	}
	if file == "" {
		file = "cursor-" + id + ".json"
	}
	body, _ := json.Marshal(doc)
	return map[string]any{
		"Handled": true,
		"Auth": map[string]any{
			"Provider": "cursor", "ID": id, "Label": label, "FileName": file, "StorageJSON": body,
		},
	}, true
}
