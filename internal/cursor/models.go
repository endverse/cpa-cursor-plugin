package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	defaultContext = 200_000
	modelsKeep     = 10 * time.Minute
	toolNS         = "tools"
	callTool       = "CallDynamicTool"
	noResult       = "Tool use was interrupted and did not produce a result."
)

var levelRank = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

var effortParams = []string{"effort", "reasoning_effort", "reasoning"}

var idEfforts = [][2]string{
	{"extra-high", "xhigh"},
	{"xhigh", "xhigh"},
	{"minimal", "minimal"},
	{"none", "none"},
	{"low", "low"},
	{"medium", "medium"},
	{"high", "high"},
	{"max", "max"},
}

var pickerSkip = map[string]bool{
	"claude-4.5-haiku":          true,
	"claude-4.5-haiku-thinking": true,
	"gemini-2.5-pro":            true,
	"gemini-2.5-flash":          true,
	"claude-haiku-4-5":          true,
}

type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flex(s)
		return nil
	}
	*f = flex(b)
	return nil
}

func (f flex) String() string { return string(f) }

type pickerModel struct {
	Name                        string     `json:"name"`
	ClientDisplayName           string     `json:"clientDisplayName"`
	IsHidden                    bool       `json:"isHidden"`
	IsChatOnly                  bool       `json:"isChatOnly"`
	OnlySupportsCmdK            bool       `json:"onlySupportsCmdK"`
	SupportsAgent               *bool      `json:"supportsAgent"`
	SupportsNonMaxMode          *bool      `json:"supportsNonMaxMode"`
	ContextTokenLimit           int64      `json:"contextTokenLimit"`
	ContextTokenLimitForMaxMode int64      `json:"contextTokenLimitForMaxMode"`
	LegacySlugs                 []string   `json:"legacySlugs"`
	IDAliases                   []string   `json:"idAliases"`
	ParameterDefinitions        []paramDef `json:"parameterDefinitions"`
	Variants                    []variant  `json:"variants"`
}

type paramDef struct {
	ID            string `json:"id"`
	ParameterType struct {
		EnumParameter *struct {
			Values []struct {
				Value flex `json:"value"`
			} `json:"values"`
		} `json:"enumParameter"`
		BooleanParameter *struct {
			Values []struct {
				Value flex `json:"value"`
			} `json:"values"`
		} `json:"booleanParameter"`
	} `json:"parameterType"`
}

type variant struct {
	LegacySlug            string `json:"legacySlug"`
	IsDefaultNonMaxConfig bool   `json:"isDefaultNonMaxConfig"`
	IsDefaultMaxConfig    bool   `json:"isDefaultMaxConfig"`
	IsMaxMode             bool   `json:"isMaxMode"`
	ParameterValues       []struct {
		ID    string `json:"id"`
		Value flex   `json:"value"`
	} `json:"parameterValues"`
}

type Param struct {
	ID    string
	Value string
}

type Entry struct {
	ID         string
	Title      string
	Context    int64
	Size       string
	Run        string
	PID        string
	Levels     map[string]string
	Fast       bool
	OffersFast bool
	Partial    bool
	Variants   []variant
	Def        *variant
	Model      pickerModel
}

type Resolved struct {
	ID      string
	Params  []Param
	MaxMode bool
	Context int64
	Title   string
	Levels  []string
}

func paramDefs(m pickerModel) map[string][]string {
	out := map[string][]string{}
	for _, d := range m.ParameterDefinitions {
		if d.ID == "" {
			continue
		}
		var vals []string
		if d.ParameterType.EnumParameter != nil {
			for _, v := range d.ParameterType.EnumParameter.Values {
				vals = append(vals, v.Value.String())
			}
		} else if d.ParameterType.BooleanParameter != nil {
			for _, v := range d.ParameterType.BooleanParameter.Values {
				vals = append(vals, v.Value.String())
			}
		}
		out[d.ID] = vals
	}
	return out
}

func valuesOf(v *variant) map[string]string {
	if v == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(v.ParameterValues))
	for _, p := range v.ParameterValues {
		out[p.ID] = p.Value.String()
	}
	return out
}

func tokensOf(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	mul := int64(1)
	last := value[len(value)-1]
	num := value
	switch last {
	case 'k', 'K':
		mul = 1000
		num = value[:len(value)-1]
	case 'm', 'M':
		mul = 1_000_000
		num = value[:len(value)-1]
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	return int64(f * float64(mul))
}

func defaultVariant(vs []variant) *variant {
	for i := range vs {
		if vs[i].IsDefaultNonMaxConfig {
			return &vs[i]
		}
	}
	for i := range vs {
		if vs[i].IsDefaultMaxConfig {
			return &vs[i]
		}
	}
	if len(vs) == 0 {
		return nil
	}
	return &vs[0]
}

func needsMax(m pickerModel, v *variant) bool {
	if m.SupportsNonMaxMode != nil && !*m.SupportsNonMaxMode {
		return true
	}
	return v != nil && v.IsMaxMode
}

func effortsOf(defs map[string][]string) (string, map[string]string) {
	pid := ""
	for _, p := range effortParams {
		if len(defs[p]) > 0 {
			pid = p
			break
		}
	}
	levels := map[string]string{}
	if pid == "" && contains(defs["thinking"], "true") && contains(defs["thinking"], "false") {
		return "thinking", map[string]string{"none": "false", "high": "true"}
	}
	if pid == "" {
		return "", levels
	}
	for _, v := range defs[pid] {
		l := v
		if l == "extra-high" {
			l = "xhigh"
		}
		if contains(levelRank, l) {
			levels[l] = v
		}
	}
	return pid, levels
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func served(m pickerModel, ids map[string]bool) []variant {
	vs := m.Variants
	if ids == nil {
		return vs
	}
	var got []variant
	anySlug := false
	for _, v := range vs {
		if v.LegacySlug == "" || ids[v.LegacySlug] {
			got = append(got, v)
			if v.LegacySlug != "" {
				anySlug = true
			}
		}
	}
	if anySlug {
		return got
	}
	return vs
}

func fastAt(variants []variant, size string) bool {
	for i := range variants {
		have := valuesOf(&variants[i])
		if have["fast"] == "true" && (size == "" || have["context"] == "" || have["context"] == size) {
			return true
		}
	}
	return false
}

func cleanName(s string) string {
	s = strings.ReplaceAll(s, "\u200b", "")
	return strings.Join(strings.Fields(s), " ")
}

func modelsOf(picker []pickerModel, ids map[string]bool) []Entry {
	var out []Entry
	used := map[string]bool{}
	for _, m := range picker {
		if m.Name == "" || m.IsHidden || m.IsChatOnly || m.OnlySupportsCmdK || (m.SupportsAgent != nil && !*m.SupportsAgent) || pickerSkip[m.Name] {
			continue
		}
		defs := paramDefs(m)
		variants := served(m, ids)
		partial := len(variants) < len(m.Variants)
		has := func(pid, value string) bool {
			if !partial {
				return true
			}
			any := false
			for i := range variants {
				if _, ok := valuesOf(&variants[i])[pid]; ok {
					any = true
					break
				}
			}
			if !any {
				return true
			}
			for i := range variants {
				if valuesOf(&variants[i])[pid] == value {
					return true
				}
			}
			return false
		}
		def := defaultVariant(variants)
		pid, all := effortsOf(defs)
		levels := map[string]string{}
		for k, v := range all {
			if has(pid, v) {
				levels[k] = v
			}
		}
		title := cleanName(m.ClientDisplayName)
		if title == "" {
			title = m.Name
		}
		base := Entry{
			Title: title, Run: m.Name, PID: pid, Levels: levels, Fast: len(defs["fast"]) > 0,
			Variants: variants, Def: def, Partial: partial, Model: m,
		}
		var contexts []string
		for _, c := range defs["context"] {
			if has("context", c) {
				contexts = append(contexts, c)
			}
		}
		if len(contexts) > 0 {
			for _, c := range contexts {
				id := m.Name + "@" + c
				if used[id] {
					continue
				}
				used[id] = true
				e := base
				e.ID = id
				e.Size = c
				e.Context = tokensOf(c)
				if e.Context == 0 {
					e.Context = defaultContext
				}
				e.OffersFast = fastAt(variants, c)
				out = append(out, e)
			}
			continue
		}
		if used[m.Name] {
			continue
		}
		used[m.Name] = true
		e := base
		e.ID = m.Name
		if m.Name == "default" {
			e.ID = "auto"
		}
		limit := m.ContextTokenLimit
		if needsMax(m, def) && m.ContextTokenLimitForMaxMode > 0 {
			limit = m.ContextTokenLimitForMaxMode
		}
		if limit <= 0 {
			limit = defaultContext
		}
		e.Context = limit
		e.OffersFast = fastAt(variants, "")
		out = append(out, e)
	}
	return out
}

func fitEffort(want string, levels []string) string {
	if want == "ultra" {
		want = "max"
	}
	if len(levels) == 0 || contains(levels, want) {
		return want
	}
	at := indexOf(levelRank, want)
	if at < 0 {
		return ""
	}
	best := ""
	dist := len(levelRank)
	for _, l := range levels {
		i := indexOf(levelRank, l)
		if i < 0 || l == "none" {
			continue
		}
		d := i - at
		if d < 0 {
			d = -d
		}
		if d < dist || (d == dist && i > at) {
			best = l
			dist = d
		}
	}
	return best
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

func nearest(vs []variant, want map[string]string) *variant {
	var best *variant
	most := -1
	for i := range vs {
		have := valuesOf(&vs[i])
		n := 0
		for k, v := range want {
			if have[k] == v {
				n++
			}
		}
		if have["context"] == want["context"] {
			n += 100
		}
		if n > most {
			most = n
			best = &vs[i]
		}
	}
	return best
}

func sameVariant(v variant, want map[string]string) bool {
	have := valuesOf(&v)
	keys := map[string]struct{}{}
	for k := range have {
		keys[k] = struct{}{}
	}
	for k := range want {
		keys[k] = struct{}{}
	}
	for k := range keys {
		hv, hok := have[k]
		wv, wok := want[k]
		if hok && wok && hv != wv {
			return false
		}
	}
	return true
}

func pick(entry Entry, effort string, fast bool, seed *variant) Resolved {
	def := entry.Def
	want := valuesOf(seed)
	if seed == nil {
		want = valuesOf(def)
	}
	if entry.Size != "" {
		want["context"] = entry.Size
	}
	if effort == "off" {
		effort = "none"
	}
	if entry.PID != "" && entry.PID != "thinking" && effort == "none" && entry.Levels["none"] == "" && want["thinking"] != "" {
		want["thinking"] = "false"
	} else if entry.PID != "" {
		at := ""
		if effort != "" {
			var keys []string
			for k := range entry.Levels {
				keys = append(keys, k)
			}
			at = fitEffort(effort, keys)
		}
		if at != "" && entry.Levels[at] != "" {
			want[entry.PID] = entry.Levels[at]
		}
		if seed == nil && entry.PID != "thinking" && want["thinking"] != "" && at != "" {
			want["thinking"] = "true"
		}
	}
	if entry.Fast {
		want["fast"] = "false"
		if fast {
			want["fast"] = "true"
		}
	}
	var matched *variant
	for i := range entry.Variants {
		if sameVariant(entry.Variants[i], want) {
			matched = &entry.Variants[i]
			break
		}
	}
	if matched == nil && seed != nil {
		matched = branch(entry, want)
	}
	if matched == nil && entry.Partial {
		matched = nearest(entry.Variants, want)
	}
	merged := want
	if matched != nil {
		merged = map[string]string{}
		for k, v := range want {
			merged[k] = v
		}
		for k, v := range valuesOf(matched) {
			merged[k] = v
		}
	}
	var params []Param
	for k, v := range merged {
		params = append(params, Param{ID: k, Value: v})
	}
	maxMode := false
	limit := entry.Model.ContextTokenLimit
	if matched != nil {
		maxMode = needsMax(entry.Model, matched)
	} else {
		maxMode = needsMax(entry.Model, def)
		if limit > 0 && tokensOf(entry.Size) > limit {
			maxMode = true
		}
		if limit == 0 && tokensOf(entry.Size) > 0 {
			maxMode = needsMax(entry.Model, def)
		}
	}
	id := entry.Run
	if def != nil && def.LegacySlug != "" {
		id = def.LegacySlug
	}
	if matched != nil && matched.LegacySlug != "" {
		id = matched.LegacySlug
	}
	var levels []string
	for _, l := range levelRank {
		if entry.Levels[l] != "" {
			levels = append(levels, l)
		}
	}
	return Resolved{ID: id, Params: params, MaxMode: maxMode, Context: entry.Context, Title: entry.Title, Levels: levels}
}

func branch(entry Entry, want map[string]string) *variant {
	if entry.PID == "" || entry.PID == "thinking" || entry.Def == nil {
		return nil
	}
	level := func(x string) string {
		if x == "extra-high" {
			return "xhigh"
		}
		return x
	}
	def := valuesOf(entry.Def)
	named := false
	for i := range entry.Variants {
		if level(valuesOf(&entry.Variants[i])[entry.PID]) == level(want[entry.PID]) {
			named = true
			break
		}
	}
	if !named {
		ok := true
		for k, v := range want {
			if k == entry.PID || k == "context" {
				continue
			}
			if dv, exists := def[k]; exists && dv != v {
				ok = false
				break
			}
		}
		if ok {
			return nil
		}
	}
	var vs []variant
	for i := range entry.Variants {
		have := valuesOf(&entry.Variants[i])
		if have[entry.PID] == "" {
			continue
		}
		good := true
		for k, v := range want {
			if k == entry.PID {
				continue
			}
			if hv, ok := have[k]; ok && hv != v {
				good = false
				break
			}
		}
		if good {
			vs = append(vs, entry.Variants[i])
		}
	}
	var haveLevels []string
	for i := range vs {
		haveLevels = append(haveLevels, level(valuesOf(&vs[i])[entry.PID]))
	}
	at := fitEffort(level(want[entry.PID]), haveLevels)
	if at == "" {
		return nil
	}
	for i := range vs {
		if level(valuesOf(&vs[i])[entry.PID]) == at {
			return &vs[i]
		}
	}
	return nil
}

func splitID(id string) (string, string) {
	s := id
	fast := false
	thinking := false
	effort := ""
	cut := func(suf string) (string, bool) {
		if strings.HasSuffix(s, suf) && len(s) > len(suf) {
			return s[:len(s)-len(suf)], true
		}
		return s, false
	}
	if n, ok := cut("-fast"); ok {
		s, fast = n, true
	}
	if n, ok := cut("-thinking"); ok {
		s, thinking = n, true
	}
	for _, pair := range idEfforts {
		if n, ok := cut("-" + pair[0]); ok {
			s, effort = n, pair[1]
			break
		}
	}
	if !thinking {
		if n, ok := cut("-thinking"); ok {
			s, thinking = n, true
		}
	}
	if thinking {
		s += "-thinking"
	}
	if fast {
		s += "-fast"
	}
	return s, effort
}

func named(raw []Entry, id string) (Entry, string, bool, *variant, bool) {
	seen := map[string]bool{}
	var models []Entry
	for _, e := range raw {
		if seen[e.Run] {
			continue
		}
		seen[e.Run] = true
		models = append(models, e)
	}
	at := func(e Entry, v *variant) Entry {
		size := ""
		if v != nil {
			size = valuesOf(v)["context"]
		}
		for _, x := range raw {
			if x.Run == e.Run && x.Size == size {
				return x
			}
		}
		return e
	}
	for _, e := range models {
		var vs []variant
		for _, v := range e.Variants {
			if v.LegacySlug == id {
				vs = append(vs, v)
			}
		}
		if len(vs) > 0 {
			seed := nearest(vs, valuesOf(e.Def))
			return at(e, seed), "", valuesOf(seed)["fast"] == "true", seed, true
		}
	}
	family, effort := splitID(id)
	for _, e := range models {
		var vs []variant
		for _, v := range e.Variants {
			fam, _ := splitID(v.LegacySlug)
			if v.LegacySlug != "" && fam == family {
				vs = append(vs, v)
			}
		}
		if len(vs) > 0 {
			seed := nearest(vs, valuesOf(e.Def))
			return at(e, seed), effort, valuesOf(seed)["fast"] == "true", seed, true
		}
	}
	for _, e := range models {
		m := e.Model
		if m.Name == id || contains(m.LegacySlugs, id) || contains(m.IDAliases, id) {
			return at(e, e.Def), "", false, nil, true
		}
	}
	return Entry{}, "", false, nil, false
}

func (a *Agent) resolve(entries []Entry, model, effort string, fast bool) Resolved {
	model = strings.TrimPrefix(strings.TrimSpace(model), "cursor/")
	if model == "" || model == "auto" {
		model = "auto"
	}
	for _, e := range entries {
		if e.ID == model {
			return pick(e, effort, fast, nil)
		}
	}
	if e, eff, f, seed, ok := named(entries, model); ok {
		if effort == "" {
			effort = eff
		}
		if !fast {
			fast = f
		}
		return pick(e, effort, fast, seed)
	}
	id := model
	if id == "auto" {
		id = "default"
	}
	return Resolved{ID: id, Context: defaultContext, Title: model}
}

func tokenKey(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}
