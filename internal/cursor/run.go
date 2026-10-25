package cursor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return uuidOf(b[:])
}

func (a *Agent) Chat(ctx context.Context, tok string, c Chat) <-chan Event {
	out := make(chan Event, 8)
	go func() {
		defer close(out)
		a.chat(ctx, tok, c, out)
	}()
	return out
}

func (a *Agent) chat(ctx context.Context, tok string, c Chat, out chan<- Event) {
	resolved := a.Resolve(ctx, tok, c.Model, c.Effort, c.Fast)
	base := a.agentURL(ctx, tok, false)
	err := a.once(ctx, tok, base, c, resolved, out)
	if err == nil {
		return
	}
	if regional(err.Message) {
		a.mu.Lock()
		a.h1Until = time.Time{}
		a.mu.Unlock()
		fresh := a.agentURL(ctx, tok, true)
		if fresh != base {
			if err2 := a.once(ctx, tok, fresh, c, resolved, out); err2 == nil {
				return
			} else {
				err = err2
			}
		}
	}
	if !resolved.MaxMode && maxRequired(err.Message) {
		resolved.MaxMode = true
		if err2 := a.once(ctx, tok, base, c, resolved, out); err2 == nil {
			return
		} else {
			err = err2
		}
	}
	out <- Event{Err: err}
}

func (a *Agent) once(ctx context.Context, tok, base string, c Chat, resolved Resolved, out chan<- Event) *StatusError {
	prep := prepare(c, resolved)
	events := make(chan Event, 8)
	errCh := make(chan *StatusError, 1)
	go func() {
		defer close(events)
		errCh <- a.drive(ctx, tok, base, prep, events)
	}()
	started := false
	for ev := range events {
		if ev.Err != nil && !started {
			go drain(events)
			return ev.Err
		}
		if ev.Text != "" || ev.Reasoning != "" || ev.Tool != nil || ev.Stop != "" {
			started = true
		}
		out <- ev
	}
	if err := <-errCh; err != nil && !started {
		return err
	}
	return nil
}

func drain(ch <-chan Event) {
	for range ch {
	}
}

func (a *Agent) drive(ctx context.Context, tok, base string, prep prepared, out chan<- Event) *StatusError {
	a.mu.Lock()
	force := a.ForceH1 || time.Now().Before(a.h1Until)
	a.mu.Unlock()
	if !force {
		err, answered := a.runH2(ctx, tok, base, prep, out)
		if err == nil || answered {
			return err
		}
		if h2Blocked(err) {
			a.mu.Lock()
			a.h1Until = time.Now().Add(10 * time.Minute)
			a.mu.Unlock()
		}
	}
	return a.runH1(ctx, tok, prep, out)
}

func h2Blocked(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "no answer over http/2") {
		return true
	}
	for _, c := range []string{"connection refused", "no such host", "network is unreachable", "i/o timeout"} {
		if strings.Contains(text, c) {
			return false
		}
	}
	return strings.Contains(text, "h2 is not supported") || strings.Contains(text, "alpn") || strings.Contains(text, "http2") || strings.Contains(text, "protocol error")
}

func (a *Agent) runHeaders(tok, requestID, contentType string) http.Header {
	h := a.headers(tok, contentType)
	h.Set("x-request-id", requestID)
	h.Set("accept-encoding", "identity")
	h.Set("x-cursor-streaming", "true")
	h.Set("x-cursor-agent-allowed-tools", "mcp_tool_call,get_mcp_tools_tool_call")
	return h
}

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *frameWriter) send(msg []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.w.Write(frame(msg))
	return err
}

func (a *Agent) runH2(ctx context.Context, tok, base string, prep prepared, out chan<- Event) (*StatusError, bool) {
	requestID := newID()
	pr, pw := io.Pipe()
	fw := &frameWriter{w: pw}
	url := strings.TrimRight(base, "/") + "/agent.v1.AgentService/Run"
	hctx, hcancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(hctx, http.MethodPost, url, pr)
	if err != nil {
		_ = pw.Close()
		hcancel()
		return statusErr(http.StatusBadGateway, err.Error()), false
	}
	req.Header = a.runHeaders(tok, requestID, "application/connect+proto")
	req.ContentLength = -1
	writerCtx, writerCancel := context.WithCancel(ctx)
	go func() {
		defer pw.Close()
		if err := fw.send(prep.first); err != nil {
			return
		}
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-writerCtx.Done():
				return
			case <-tick.C:
				if err := fw.send((&pb{}).bytes(7, nil).done()); err != nil {
					return
				}
			}
		}
	}()
	type head struct {
		res *http.Response
		err error
	}
	got := make(chan head, 1)
	go func() { res, err := a.H2.Do(req); got <- head{res, err} }()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		writerCancel()
		hcancel()
		return statusErr(499, "the request was cancelled"), false
	case <-timer.C:
		writerCancel()
		hcancel()
		return statusErr(http.StatusBadGateway, "no answer over HTTP/2 in 15s"), false
	case hd := <-got:
		if hd.err != nil {
			writerCancel()
			hcancel()
			if ctx.Err() != nil {
				return statusErr(499, "the request was cancelled"), false
			}
			return statusErr(http.StatusBadGateway, hd.err.Error()), false
		}
		defer hd.res.Body.Close()
		if hd.res.StatusCode < 200 || hd.res.StatusCode > 299 {
			buf, _ := io.ReadAll(io.LimitReader(hd.res.Body, 1<<20))
			writerCancel()
			hcancel()
			return failure(hd.res.StatusCode, string(buf)), true
		}
		decErr := a.decode(ctx, hd.res.Body, prep, out, fw.send, func() {
			writerCancel()
			hcancel()
		})
		return decErr, true
	}
}

func (a *Agent) runH1(ctx context.Context, tok string, prep prepared, out chan<- Event) *StatusError {
	requestID := newID()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	msgs := make(chan []byte, 32)
	var seq int
	var writeMu sync.Mutex
	appendMsg := func(msg []byte) error {
		writeMu.Lock()
		n := seq
		seq++
		writeMu.Unlock()
		body := (&pb{}).str(1, hex.EncodeToString(msg)).embed(2, (&pb{}).str(1, requestID)).varint(3, uint64(n)).done()
		actx, acancel := context.WithTimeout(ctx, 60*time.Second+time.Duration(len(body)/65536)*time.Second)
		defer acancel()
		req, err := http.NewRequestWithContext(actx, http.MethodPost, strings.TrimRight(a.API, "/")+"/aiserver.v1.BidiService/BidiAppend", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header = a.runHeaders(tok, requestID, "application/proto")
		res, err := a.Client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		buf, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return failure(res.StatusCode, string(buf))
		}
		return nil
	}
	go func() {
		queue := [][]byte{prep.first}
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			if len(queue) == 0 {
				select {
				case <-ctx.Done():
					return
				case m, ok := <-msgs:
					if !ok {
						return
					}
					queue = append(queue, m)
					continue
				case <-tick.C:
					queue = append(queue, (&pb{}).bytes(7, nil).done())
					continue
				}
			}
			msg := queue[0]
			queue = queue[1:]
			if err := appendMsg(msg); err != nil {
				cancel()
				return
			}
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.API, "/")+"/agent.v1.AgentService/RunSSE", bytes.NewReader(frame((&pb{}).str(1, requestID).done())))
	if err != nil {
		return statusErr(http.StatusBadGateway, err.Error())
	}
	req.Header = a.runHeaders(tok, requestID, "application/connect+proto")
	res, err := a.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return statusErr(499, "the request was cancelled")
		}
		return statusErr(http.StatusBadGateway, err.Error())
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		buf, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return failure(res.StatusCode, string(buf))
	}
	return a.decode(ctx, res.Body, prep, out, func(msg []byte) error {
		select {
		case msgs <- msg:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, cancel)
}

func (a *Agent) decode(ctx context.Context, body io.Reader, prep prepared, out chan<- Event, send func([]byte) error, closeRun func()) *StatusError {
	defer closeRun()
	calls := 0
	answered := 0
	listed := 0
	wrote := 0
	said := 0
	finished := false
	usage := Usage{}
	var terminal *StatusError
	stepDone := func() bool {
		if listed == 0 || calls+answered < listed {
			return false
		}
		if calls > 0 {
			return true
		}
		listed = 0
		answered = 0
		return false
	}
	finish := func() {
		if finished {
			return
		}
		finished = true
		if usage.Output == 0 {
			usage.Output = (said + 3) / 4
		}
		if usage.Input == 0 && usage.CacheRead == 0 && usage.CacheWrite == 0 {
			usage.Input = prep.est
		}
		stop := "stop"
		if calls > 0 {
			stop = "tool_calls"
		}
		u := usage
		out <- Event{Stop: stop, Usage: &u}
	}
	closeExec := func(id uint64) {
		_ = send((&pb{}).embed(5, (&pb{}).embed(1, (&pb{}).varint(1, id))).done())
	}
	err := readFrames(body, func(end bool, data []byte) error {
		if end {
			e := failure(200, string(data))
			if e.Status < 200 || e.Status > 299 {
				terminal = e
				return io.EOF
			}
			if wrote == 0 && calls == 0 {
				terminal = statusErr(http.StatusBadGateway, "an empty reply")
				return io.EOF
			}
			finish()
			return io.EOF
		}
		for _, m := range fields(data) {
			switch m.num {
			case 1:
				for _, u := range fields(m.data) {
					uf := fields(u.data)
					switch u.num {
					case 1:
						if t := pbStr(uf, 1); t != "" {
							said += len(t)
							wrote += len(t)
							out <- Event{Text: t}
						}
					case 4:
						if t := pbStr(uf, 1); t != "" {
							said += len(t)
							out <- Event{Reasoning: t}
						}
					case 14:
						usage = usageOf(uf)
						if calls == 0 {
							if wrote == 0 {
								terminal = statusErr(http.StatusBadGateway, "an empty reply")
							} else {
								finish()
							}
							return io.EOF
						}
					case 27:
						listed = int(pbNum(uf, 1))
						if stepDone() {
							finish()
							return io.EOF
						}
					}
				}
			case 2:
				call, counted := execMessage(m.data, prep.tools, send, closeExec)
				if call != nil {
					said += len(call.Args)
					out <- Event{Tool: call}
					calls++
				} else if counted {
					answered++
				}
				if stepDone() {
					finish()
					return io.EOF
				}
			case 4:
				answerBlob(m.data, prep.blobs, send)
			}
		}
		return nil
	})
	if terminal != nil && wrote == 0 && calls == 0 {
		return terminal
	}
	if finished {
		return nil
	}
	if err != nil && err != io.EOF && ctx.Err() == nil && calls == 0 && wrote == 0 {
		return statusErr(http.StatusBadGateway, "the reply broke off: "+err.Error())
	}
	if wrote == 0 && calls == 0 {
		return statusErr(http.StatusBadGateway, "the reply broke off: EOF")
	}
	finish()
	return nil
}

func usageOf(uf []field) Usage {
	inp := int(pbNum(uf, 1))
	cr := int(pbNum(uf, 3))
	cw := int(pbNum(uf, 4))
	in := inp - cr - cw
	if in < 0 {
		in = 0
	}
	return Usage{Input: in, Output: int(pbNum(uf, 2)), CacheRead: cr, CacheWrite: cw, Reasoning: int(pbNum(uf, 5))}
}

func answerBlob(data []byte, blobs map[string][]byte, send func([]byte) error) {
	kv := fields(data)
	id := pbNum(kv, 1)
	for _, k := range kv {
		switch k.num {
		case 2:
			want := ""
			if f := fieldBy(fields(k.data), 1); f != nil {
				want = hex.EncodeToString(f.data)
			}
			var res *pb
			if b, ok := blobs[want]; ok {
				res = (&pb{}).bytes(1, b)
			} else {
				res = (&pb{}).embed(2, (&pb{}).str(1, "blob not found"))
			}
			_ = send((&pb{}).embed(3, (&pb{}).varint(1, id).embed(2, res)).done())
		case 3:
			_ = send((&pb{}).embed(3, (&pb{}).varint(1, id).bytes(3, nil)).done())
		}
	}
}

// execMessage returns the caller's tool, or nil. counted is true when the
// answer occupies one of the step's listed calls (a refusal or the tool list).
func execMessage(msg []byte, tools []Tool, send func([]byte) error, closeExec func(uint64)) (*ToolCall, bool) {
	es := fields(msg)
	id := pbNum(es, 1)
	execID := pbStr(es, 15)
	answer := func(num int, res *pb) {
		_ = send((&pb{}).embed(2, (&pb{}).varint(1, id).str(15, execID).embed(num, res)).done())
		closeExec(id)
	}
	unavailable := func() (*ToolCall, bool) {
		_ = send((&pb{}).embed(5, (&pb{}).embed(2, (&pb{}).varint(1, id).str(2, "not available"))).done())
		closeExec(id)
		return nil, true
	}
	for _, e := range es {
		switch e.num {
		case 11:
			call, reject := mcpCall(e.data)
			if reject || call == nil {
				return unavailable()
			}
			return call, false
		case 36:
			srv := (&pb{}).str(1, toolNS).str(2, toolNS).str(7, "connected")
			for _, t := range tools {
				srv.bytes(5, toolDef(t))
			}
			answer(36, (&pb{}).embed(1, (&pb{}).embed(1, srv)))
			return nil, true
		case 10:
			answer(10, (&pb{}).embed(1, (&pb{}).embed(1, (&pb{}).bytes(4, envMessage()))))
			return nil, false
		}
	}
	return unavailable()
}

func mcpCall(data []byte) (*ToolCall, bool) {
	a := fields(data)
	provider := pbStr(a, 4)
	server := pbStr(a, 9)
	if (provider != "" && provider != toolNS && provider != "magpie") || (server != "" && server != toolNS && server != "magpie") {
		return nil, true
	}
	args := map[string]any{}
	for _, kv := range a {
		if kv.num != 2 {
			continue
		}
		ef := fields(kv.data)
		key := pbStr(ef, 1)
		if v := fieldBy(ef, 2); v != nil {
			args[key] = pbAny(v.data)
		} else {
			args[key] = nil
		}
	}
	name := pbStr(a, 5)
	if name == "" {
		name = bareName(pbStr(a, 1))
	}
	if name == "" {
		return nil, true
	}
	raw, _ := json.Marshal(args)
	cid := strings.ReplaceAll(pbStr(a, 3), "\n", "__")
	if cid == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		cid = "call_" + hex.EncodeToString(b[:])
	}
	return &ToolCall{ID: cid, Name: name, Args: string(raw)}, false
}

func bareName(s string) string {
	for _, ns := range []string{toolNS, "magpie"} {
		if strings.HasPrefix(s, ns+"-") {
			return s[len(ns)+1:]
		}
	}
	return s
}
