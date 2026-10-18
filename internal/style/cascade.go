package style

import (
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"

	"golang.org/x/net/html"

	snet "github.com/sweefbrowser/sweef/internal/net"

	"github.com/sweefbrowser/sweef/internal/font"
)

// SheetIn is one author stylesheet handed to NewEngine.
type SheetIn struct {
	Text  string
	Media string
}

// Engine matches rules and computes styles for one document.
type Engine struct {
	env       *Env
	rules     []*Rule
	byID      map[string][]*Entry
	byClass   map[string][]*Entry
	byTag     map[string][]*Entry
	univ      []*Entry
	Fonts     []FontFace
	SysFont   string
	Quirks    bool
	RootFont  float64
	initial   *Computed
	inline    map[string][]Decl
	share     map[uint64]*Computed
	scratch   []*Entry
	hasPseudo bool
	// genContent lists, per pseudo-element ("before", "after"), the entries whose rule declares content: only
	// those can make the pseudo-element generate a box (HasGeneratedContent). Bucketed like byID/byClass/byTag,
	// so an element only tries the entries its id, classes and tag can match.
	genContent map[string]*entryIndex
	contSize   map[*html.Node]ContSize
	conts      map[*html.Node]*contInfo
	hasCQ      bool
	cqEvals    uint64 // container conditions evaluated so far (Persist: which styles depend on container sizes)
	cqUsed     bool   // a cq* unit was resolved since Persist last cleared it
	depTried   bool   // collect tried a :has() entry since Persist last cleared it
	curCont    *html.Node
	persist    *Persist     // cross-pass style cache (persist.go); nil: none
	web        *font.WebSet // the document's web fonts: declared families get scoped names (webfonts.go)
	// usesWebMetrics: some length was computed from the metrics of a web font (webfonts.go)
	usesWebMetrics bool
	// anc memoizes the ancestor filter of the elements matched in this pass (BeginPass; nil: no filter, for an
	// engine that outlives a DOM mutation); ancN/ancB is the last answer (collect asks for one element at a time)
	anc  map[*html.Node]snet.Bloom
	ancN *html.Node
	ancB snet.Bloom
	// container conditions evaluated this pass (ContainerResultsStable); cqLogOff: the log is incomplete
	cqLog       []cqRec
	cqLogOff    bool
	cqPassUnits bool // a container unit was resolved this pass
}

// BeginPass starts a style pass over a document that does not change until the next BeginPass: matching may
// then use the ancestor filter, memoized per element.
func (e *Engine) BeginPass() {
	e.anc = map[*html.Node]snet.Bloom{}
	e.ancN = nil
	e.cqLog, e.cqLogOff, e.cqPassUnits = nil, false, false
}

// ancBloom returns the union of NodeBloom over n's element ancestors.
func (e *Engine) ancBloom(n *html.Node) snet.Bloom {
	if n == e.ancN {
		return e.ancB
	}
	b := AncestorBloom(n, e.anc)
	e.ancN, e.ancB = n, b
	return b
}

// AncestorBloom returns the union of snet.NodeBloom over n's element ancestors, memoized in memo (keyed by the
// element whose ancestors were hashed).
func AncestorBloom(n *html.Node, memo map[*html.Node]snet.Bloom) snet.Bloom {
	p := n.Parent
	if p == nil || p.Type != html.ElementNode {
		return snet.Bloom{}
	}
	if b, ok := memo[n]; ok {
		return b
	}
	b := AncestorBloom(p, memo)
	b.Add(snet.NodeBloom(p))
	memo[n] = b
	return b
}

var (
	uaOnce  sync.Once
	uaRules []*Rule
)

func uaParsed() []*Rule {
	uaOnce.Do(func() {
		order := 0
		p := ParseSheet(uaCSS, &Env{Width: 1280, Height: 800, DPR: 1, ColorScheme: "light"}, OriginUA, &order, map[string]int{})
		uaRules = p.Rules
	})
	return uaRules
}

// NewEngine builds an engine from author sheets (UA sheet is built in).
func NewEngine(env *Env, sheets []SheetIn) *Engine { return NewEngineQ(env, sheets, false) }

const quirkCSS = `form { margin-bottom: 1em } table { white-space: normal; line-height: normal; font-weight: normal; font-size: medium; font-variant: normal; font-style: normal; text-align: start }`

// NewEngineQ is NewEngine with an optional quirks-mode user-agent sheet.
func NewEngineQ(env *Env, sheets []SheetIn, quirks bool) *Engine {
	e := &Engine{env: env, byID: map[string][]*Entry{}, byClass: map[string][]*Entry{}, byTag: map[string][]*Entry{},
		SysFont: "system-ui", RootFont: 16, inline: map[string][]Decl{}, share: map[uint64]*Computed{}}
	e.initial = Initial()
	for _, r := range uaParsed() {
		e.addRule(r)
	}
	e.Quirks = quirks
	if quirks {
		o := 1
		p := ParseSheet(quirkCSS, env, OriginUA, &o, map[string]int{})
		for _, r := range p.Rules {
			e.addRule(r)
		}
	}
	order := 1000000
	layers := map[string]int{}
	for _, sh := range sheets {
		if sh.Media != "" && !MediaMatches(sh.Media, env) {
			continue
		}
		p := ParseSheetQ(sh.Text, env, OriginAuthor, &order, layers, quirks)
		for _, r := range p.Rules {
			e.addRule(r)
		}
		e.Fonts = append(e.Fonts, p.Fonts...)
	}
	return e
}

// entryIndex is a set of entries bucketed by the rightmost compound of their selector (see bucketKey).
type entryIndex struct {
	byID, byClass, byTag map[string][]*Entry
	univ                 []*Entry
}

func (ix *entryIndex) add(en *Entry, k byte, v string) {
	switch k {
	case 'i':
		if ix.byID == nil {
			ix.byID = map[string][]*Entry{}
		}
		ix.byID[v] = append(ix.byID[v], en)
	case 'c':
		if ix.byClass == nil {
			ix.byClass = map[string][]*Entry{}
		}
		ix.byClass[v] = append(ix.byClass[v], en)
	case 't':
		if ix.byTag == nil {
			ix.byTag = map[string][]*Entry{}
		}
		ix.byTag[v] = append(ix.byTag[v], en)
	default:
		ix.univ = append(ix.univ, en)
	}
}

func (e *Engine) systemFont() string { return e.SysFont }

func (e *Engine) addRule(r *Rule) {
	e.rules = append(e.rules, r)
	for _, en := range r.Entries {
		k, v := bucketKey(en.key)
		if e.Quirks && (k == 'i' || k == 'c') {
			v = strings.ToLower(v)
		}
		switch k {
		case 'i':
			e.byID[v] = append(e.byID[v], en)
		case 'c':
			e.byClass[v] = append(e.byClass[v], en)
		case 't':
			e.byTag[v] = append(e.byTag[v], en)
		default:
			e.univ = append(e.univ, en)
		}
		if en.Pseudo != "" {
			e.hasPseudo = true
			if (en.Pseudo == "before" || en.Pseudo == "after") && declaresContent(r.Decls) {
				if e.genContent == nil {
					e.genContent = map[string]*entryIndex{}
				}
				ix := e.genContent[en.Pseudo]
				if ix == nil {
					ix = &entryIndex{}
					e.genContent[en.Pseudo] = ix
				}
				ix.add(en, k, v)
			}
		}
		if r.Cont != nil {
			e.hasCQ = true
		}
	}
}

// bucketKey finds an index key from the rightmost compound of a selector.
func bucketKey(sel string) (byte, string) {
	// find the last combinator at depth 0
	depth := 0
	start := 0
	for i := 0; i < len(sel); i++ {
		switch sel[i] {
		case '\\':
			i++
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ' ', '>', '+', '~':
			if depth == 0 {
				start = i + 1
			}
		}
	}
	comp := sel[start:]
	var id, class, tag string
	for i := 0; i < len(comp); {
		c := comp[i]
		switch c {
		case '#', '.':
			j := i + 1
			for j < len(comp) && identChar(comp, j) {
				if comp[j] == '\\' {
					j++
				}
				j++
			}
			if j > len(comp) {
				j = len(comp)
			}
			name := unescapeStr(comp[i+1 : j])
			if c == '#' {
				id = name
			} else if class == "" {
				class = name
			}
			i = j
		case '[', '(':
			d := 0
			for ; i < len(comp); i++ {
				if comp[i] == '[' || comp[i] == '(' {
					d++
				} else if comp[i] == ']' || comp[i] == ')' {
					d--
					if d == 0 {
						i++
						break
					}
				}
			}
		case ':':
			i++
			for i < len(comp) && (comp[i] == ':' || identChar(comp, i)) {
				i++
			}
		case '*':
			i++
		default:
			j := i
			for j < len(comp) && identChar(comp, j) {
				if comp[j] == '\\' {
					j++
				}
				j++
			}
			if j == i {
				j = i + 1
			}
			if i == 0 && j <= len(comp) {
				tag = strings.ToLower(comp[i:j])
			}
			i = j
		}
	}
	switch {
	case id != "":
		return 'i', id
	case class != "":
		return 'c', class
	case tag != "":
		return 't', tag
	}
	return 0, ""
}

// AnimAttr is the host attribute holding an element's Web Animations values ("prop: value; ...").
const AnimAttr = "data-sw-anim"

func identChar(s string, i int) bool {
	c := s[i]
	return c == '-' || c == '_' || c == '\\' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func attrOf(n *html.Node, k string) (string, bool) {
	for i := range n.Attr {
		if n.Attr[i].Key == k {
			return n.Attr[i].Val, true
		}
	}
	return "", false
}

func declaresContent(ds []Decl) bool {
	for _, d := range ds {
		if d.Name == "content" {
			return true
		}
	}
	return false
}

// HasGeneratedContent reports whether some rule that declares content matches the ::before or ::after
// (pseudo "before" / "after") of n. Without one the pseudo-element's content is normal and it generates no
// box, so layout can skip computing its style: a reset like `*, ::before, ::after { box-sizing: border-box }`
// otherwise makes every element of the page compute two pseudo-element styles that are thrown away.
func (e *Engine) HasGeneratedContent(n *html.Node, pseudo string) bool {
	ix := e.genContent[pseudo]
	if ix == nil {
		return false
	}
	try := func(list []*Entry) bool {
		for _, en := range list {
			if e.entryMatches(n, en, pseudo) {
				return true
			}
		}
		return false
	}
	if len(ix.byID) > 0 {
		if id, ok := attrOf(n, "id"); ok && id != "" {
			if e.Quirks {
				id = strings.ToLower(id)
			}
			if try(ix.byID[id]) {
				return true
			}
		}
	}
	if len(ix.byClass) > 0 {
		if cl, ok := attrOf(n, "class"); ok && cl != "" {
			for _, c := range strings.Fields(cl) {
				if e.Quirks {
					c = strings.ToLower(c)
				}
				if try(ix.byClass[c]) {
					return true
				}
			}
		}
	}
	return try(ix.byTag[n.Data]) || try(ix.univ)
}

// entryMatches: en applies to n (selector, @scope and @container conditions); the caller checked the pseudo.
func (e *Engine) entryMatches(n *html.Node, en *Entry, pseudo string) bool {
	var scopeEl *html.Node
	if en.Rule.Scope != nil {
		if scopeEl = scopeRootFor(n, en.Rule.Scope); scopeEl == nil {
			return false
		}
		if scopeEl == n && !strings.Contains(en.key, ":scope") {
			return false
		}
	}
	if en.need != (snet.Bloom{}) && e.anc != nil && !en.need.Subset(e.ancBloom(n)) {
		return false
	}
	if !en.Sel.Match(n, scopeEl) {
		return false
	}
	return en.Rule.Cont == nil || e.contMatches(n, en.Rule.Cont, pseudo)
}

// collectDyn is collect restricted to the entries whose match the style cache's keys do not determine (:has()
// selectors, container conditions); see Persist.depSig.
func (e *Engine) collectDyn(n *html.Node, pseudo string, visit func(*Entry)) {
	try := func(list []*Entry) {
		for _, en := range list {
			if en.Pseudo != pseudo || !(en.dynamic || en.Rule.Cont != nil) {
				continue
			}
			if e.entryMatches(n, en, pseudo) {
				visit(en)
			}
		}
	}
	if id, ok := attrOf(n, "id"); ok && id != "" {
		if e.Quirks {
			id = strings.ToLower(id)
		}
		try(e.byID[id])
	}
	if cl, ok := attrOf(n, "class"); ok && cl != "" {
		for _, c := range strings.Fields(cl) {
			if e.Quirks {
				c = strings.ToLower(c)
			}
			try(e.byClass[c])
		}
	}
	try(e.byTag[n.Data])
	try(e.univ)
}

func (e *Engine) collect(n *html.Node, pseudo string) []*Entry {
	out := e.scratch[:0]
	try := func(list []*Entry) {
		for _, en := range list {
			if en.Pseudo != pseudo {
				continue
			}
			if en.dynamic {
				e.depTried = true
			}
			if e.entryMatches(n, en, pseudo) {
				out = append(out, en)
			}
		}
	}
	if id, ok := attrOf(n, "id"); ok && id != "" {
		if e.Quirks {
			id = strings.ToLower(id)
		}
		try(e.byID[id])
	}
	if cl, ok := attrOf(n, "class"); ok && cl != "" {
		for _, c := range strings.Fields(cl) {
			if e.Quirks {
				c = strings.ToLower(c)
			}
			try(e.byClass[c])
		}
	}
	try(e.byTag[n.Data])
	try(e.univ)
	e.scratch = out
	return out
}

func layerKey(r *Rule) int {
	if r.Layer == 0 {
		return 1 << 30
	}
	return r.Layer
}

// Compute returns the style of element n (pseudo "" ) or of one of its
// pseudo-elements. parent is the parent element's computed style (nil for the
// root). A nil result for a pseudo means no rule applies.
func (e *Engine) Compute(n *html.Node, parent *Computed, pseudo string) *Computed {
	if e.persist != nil {
		return e.persist.compute(e, n, parent, pseudo)
	}
	return e.computeNoCache(n, parent, pseudo)
}

// computeNoCache is Compute without the cross-pass cache (persist.go).
func (e *Engine) computeNoCache(n *html.Node, parent *Computed, pseudo string) *Computed {
	c := e.compute(n, parent, pseudo)
	if c != nil && pseudo == "" && c.ContainerType != "" {
		e.noteContainer(n, c)
	}
	return c
}

func (e *Engine) compute(n *html.Node, parent *Computed, pseudo string) *Computed {
	if parent == nil {
		parent = e.initial
	}
	var ents []*Entry
	if pseudo != "" && !e.hasPseudo {
		return nil
	}
	ents = e.collect(n, pseudo)
	if pseudo != "" && len(ents) == 0 {
		return nil
	}
	// style sharing
	var key uint64
	canShare := false
	var styleAttr string
	var hasStyle bool
	if pseudo == "" {
		styleAttr, hasStyle = attrOf(n, "style")
		if _, anim := attrOf(n, AnimAttr); !hasStyle && !anim && !needsHints(n) {
			canShare = true
		}
	}
	// stable order of matched entries
	sort.SliceStable(ents, func(i, j int) bool {
		a, b := ents[i], ents[j]
		ra, rb := a.Rule, b.Rule
		if ra.Origin != rb.Origin {
			return ra.Origin < rb.Origin
		}
		if ra.Origin == OriginAuthor && ra.Layer != rb.Layer {
			return layerKey(ra) < layerKey(rb)
		}
		if a.Spec != b.Spec {
			return a.Spec < b.Spec
		}
		return ra.Order < rb.Order
	})
	if canShare {
		h := fnv.New64a()
		var buf [8]byte
		put := func(v uint64) {
			for i := 0; i < 8; i++ {
				buf[i] = byte(v >> (8 * i))
			}
			h.Write(buf[:])
		}
		put(uint64(ptrID(parent)))
		h.Write([]byte(n.Data))
		if e.hasCQ || len(e.conts) > 0 {
			if an, _ := e.nearestContainer(n, "", false); an != nil {
				sz := e.contSize[an]
				put(math.Float64bits(sz.W))
				put(math.Float64bits(sz.H))
			}
		}
		for _, en := range ents {
			put(uint64(en.Rule.Order)<<8 | uint64(en.Rule.Origin))
			put(uint64(en.Spec))
		}
		key = h.Sum64()
		if c, ok := e.share[key]; ok {
			return c
		}
	}
	// declaration list
	var normal []Decl
	var impl []Decl
	var impRules []*Rule
	last := (*Rule)(nil)
	hintDone := false
	uaEnd := -1
	flushHints := func() {
		if hintDone {
			return
		}
		hintDone = true
		if uaEnd < 0 {
			uaEnd = len(normal)
		}
		if pseudo == "" {
			normal = append(normal, e.hints(n)...)
		}
	}
	for _, en := range ents {
		r := en.Rule
		if r == last {
			continue
		}
		last = r
		if r.Origin != OriginUA && uaEnd < 0 {
			uaEnd = len(normal)
		}
		if r.Origin == OriginAuthor {
			flushHints()
		}
		for _, d := range r.Decls {
			if d.Imp {
				continue
			}
			normal = append(normal, d)
		}
		if r.hasImportant() {
			impRules = append(impRules, r)
		}
	}
	flushHints()
	var inl []Decl
	if hasStyle {
		var ok bool
		inl, ok = e.inline[styleAttr]
		if !ok {
			inl = ParseInline(styleAttr)
			e.inline[styleAttr] = inl
		}
		for _, d := range inl {
			if !d.Imp {
				normal = append(normal, d)
			}
		}
	}
	// Web Animations: the element's animated values (the engine keeps them in AnimAttr) come after its normal
	// declarations, inline style included, as the animation origin of the cascade does
	if pseudo == "" {
		if av, ok := attrOf(n, AnimAttr); ok && av != "" {
			ad, ok := e.inline[av]
			if !ok {
				ad = ParseInline(av)
				e.inline[av] = ad
			}
			for _, d := range ad {
				if !d.Imp {
					normal = append(normal, d)
				}
			}
		}
	}
	qTop, qBot := false, false
	if e.Quirks && pseudo == "" && quirkMarginTag(n.Data) {
		qTop, qBot = true, true
		scan := func(ds []Decl) {
			for _, d := range ds {
				switch d.Name {
				case "margin", "margin-block":
					qTop, qBot = false, false
				case "margin-top", "margin-block-start":
					qTop = false
				case "margin-bottom", "margin-block-end":
					qBot = false
				}
			}
		}
		if uaEnd >= 0 && uaEnd <= len(normal) {
			scan(normal[uaEnd:])
		}
		scan(inl)
	}
	if len(impRules) > 1 {
		// !important reverses the layer order: unlayered is weakest, then the
		// last layer ... the first layer
		impKey := func(r *Rule) int {
			if r.Origin != OriginAuthor {
				return -1
			}
			if r.Layer == 0 {
				return 0
			}
			return 1<<20 - r.Layer
		}
		sort.SliceStable(impRules, func(i, j int) bool { return impKey(impRules[i]) < impKey(impRules[j]) })
	}
	var uaImp []Decl
	for _, r := range impRules {
		for _, d := range r.Decls {
			if !d.Imp {
				continue
			}
			if r.Origin == OriginUA {
				uaImp = append(uaImp, d)
			} else {
				impl = append(impl, d)
			}
		}
	}
	for _, d := range inl {
		if d.Imp {
			impl = append(impl, d)
		}
	}
	impl = append(impl, uaImp...)
	decls := normal
	if len(impl) > 0 {
		decls = append(decls, impl...)
	}
	e.curCont = nil
	if len(e.conts) > 0 {
		if pseudo == "" {
			e.curCont, _ = e.nearestContainer(n, "", false)
		} else {
			e.curCont, _ = e.nearestContainer(n, "", false)
			if _, ok := e.conts[n]; ok {
				e.curCont = n
			}
		}
	}
	c := e.computeFrom(decls, parent, pseudo)
	c.QuirkMarginTop, c.QuirkMarginBottom = qTop, qBot
	if canShare {
		e.share[key] = c
	}
	return c
}

func (r *Rule) hasImportant() bool {
	for i := range r.Decls {
		if r.Decls[i].Imp {
			return true
		}
	}
	return false
}

var ptrSeq uint64
var ptrMu sync.Mutex

// ptrID gives each Computed a small stable identity for the sharing key.
func ptrID(c *Computed) uint64 {
	if c.id != 0 {
		return c.id
	}
	ptrMu.Lock()
	ptrSeq++
	c.id = ptrSeq
	ptrMu.Unlock()
	return c.id
}

// ---- compute ---------------------------------------------------------------------------

func (e *Engine) newComputed(parent *Computed) *Computed {
	c := *e.initial
	c.id = 0
	c.Inherited = parent.Inherited
	return &c
}

func isFontGroup(name string) bool {
	switch name {
	case "font", "font-family", "font-size", "font-weight", "font-style", "font-variant", "font-variant-caps", "font-stretch", "color", "zoom", "direction":
		return true
	}
	return false
}

func (e *Engine) computeFrom(decls []Decl, parent *Computed, pseudo string) *Computed {
	c := e.newComputed(parent)
	s := &cstate{c: c, p: parent, env: e.env, eng: e}
	s.uc = unitCtx{em: parent.FontSize, rem: e.RootFont, vw: e.env.Width, vh: e.env.Height}
	s.uc.containerW, s.uc.cqH = e.env.Width, e.env.Height
	s.uc.cqUsed = &e.cqUsed
	if e.curCont != nil {
		sz := e.contSize[e.curCont]
		s.uc.containerW, s.uc.cqH = sz.W, sz.H
	}
	// custom properties
	vars := parent.Custom
	own := false
	for i := range decls {
		d := &decls[i]
		if len(d.Name) > 2 && d.Name[0] == '-' && d.Name[1] == '-' {
			if !own {
				nv := make(map[string]string, len(vars)+4)
				for k, v := range vars {
					nv[k] = v
				}
				vars = nv
				own = true
			}
			if d.L == "initial" {
				delete(vars, d.Name)
			} else if d.L != "inherit" {
				vars[d.Name] = d.Val
			}
		}
	}
	c.Custom = vars
	s.vars = vars
	lastFont := -1
	for i := range decls {
		if decls[i].Name == "font" {
			lastFont = i
		}
	}
	for pass := 0; pass < 2; pass++ {
		for i := range decls {
			d := &decls[i]
			if len(d.Name) > 1 && d.Name[0] == '-' && d.Name[1] == '-' {
				continue
			}
			if isFontGroup(d.Name) != (pass == 0) {
				continue
			}
			if pass == 1 && d.Name == "line-height" && i < lastFont {
				continue
			}
			s.applyDecl(d)
		}
		if pass == 0 {
			s.fixMonospace()
			if parent == e.initial && pseudo == "" {
				e.RootFont = c.FontSize
				s.uc.rem = c.FontSize
			}
			s.uc.em = c.FontSize
			s.metricsDone = false
		}
	}
	e.finalize(c, parent, pseudo)
	return c
}

func (s *cstate) applyDecl(d *Decl) {
	v, l := d.Val, d.L
	subst := false
	if strings.Contains(v, "var(") || strings.Contains(v, "env(") {
		nv, ok := s.substVars(v, 0)
		if !ok {
			s.global(d.Name, "unset")
			return
		}
		v, l, subst = nv, strings.ToLower(nv), true
	}
	if len(l) <= 13 {
		if g := isGlobal(l); g != "" {
			s.global(d.Name, g)
			return
		}
	}
	if !s.apply(d.Name, v, l) && subst {
		s.global(d.Name, "unset")
	}
}

// substVars replaces var() and env() references.
func (s *cstate) substVars(v string, depth int) (string, bool) {
	if depth > 16 {
		return "", false
	}
	for {
		i := strings.Index(v, "var(")
		j := strings.Index(v, "env(")
		isEnv := false
		if i < 0 || (j >= 0 && j < i) {
			i = j
			isEnv = true
		}
		if i < 0 {
			return v, true
		}
		// ensure identifier boundary
		if i > 0 && (isIdentByte(v[i-1])) {
			// part of another name (e.g. "xvar(") -- skip by cutting
			rest, ok := s.substVars(v[i+4:], depth)
			return v[:i+4] + rest, ok
		}
		// find matching paren
		d := 0
		k := i + 3
		end := -1
		for ; k < len(v); k++ {
			if v[k] == '(' {
				d++
			} else if v[k] == ')' {
				d--
				if d == 0 {
					end = k
					break
				}
			}
		}
		if end < 0 {
			return "", false
		}
		args := v[i+4 : end]
		var name, fallback string
		hasFallback := false
		if c := strings.IndexByte(args, ','); c >= 0 {
			name, fallback, hasFallback = strings.TrimSpace(args[:c]), strings.TrimSpace(args[c+1:]), true
		} else {
			name = strings.TrimSpace(args)
		}
		var rep string
		if isEnv {
			if hasFallback {
				rep = fallback
			} else {
				rep = "0px"
			}
		} else {
			val, ok := s.vars[name]
			if ok {
				r, ok2 := s.substVars(val, depth+1)
				if !ok2 {
					return "", false
				}
				rep = r
			} else if hasFallback {
				r, ok2 := s.substVars(fallback, depth+1)
				if !ok2 {
					return "", false
				}
				rep = r
			} else {
				return "", false
			}
		}
		v = v[:i] + rep + v[end+1:]
	}
}

func isIdentByte(c byte) bool {
	return c == '-' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func (e *Engine) finalize(c, parent *Computed, pseudo string) {
	for i := 0; i < 4; i++ {
		if !c.bcSet[i] {
			c.BorderColor[i] = c.Color
		}
		if c.BorderStyle[i] == "none" || c.BorderStyle[i] == "hidden" {
			c.BorderWidth[i] = 0
		}
	}
	if !c.outlineSet {
		c.OutlineColor = c.Color
	}
	if c.OutlineStyle == "none" {
		c.OutlineWidth = 0
	}
	if c.ColumnRuleStyle == "none" || c.ColumnRuleStyle == "hidden" {
		c.ColumnRuleW = 0
	}
	if c.TextStrokeSet == false {
		c.TextStrokeC = c.Color
	}
	if !c.TextFillSet {
		c.TextFill = c.Color
	}
	if (c.Display == "table" || c.Display == "inline-table") && strings.HasPrefix(c.TextAlign, "-webkit-") && c.TextAlign == parent.TextAlign {
		c.TextAlign = "start"
	}
	c.OrigInline = c.Display == "inline" || strings.HasPrefix(c.Display, "inline-") || c.Display == "contents"
	// blockification
	if c.Float != "none" || c.Position == "absolute" || c.Position == "fixed" {
		c.Display = blockify(c.Display)
	}
	if c.OverflowX == "visible" && c.OverflowY == "visible" && (containTok(c.Contain, "paint") || containTok(c.Contain, "strict") || containTok(c.Contain, "content") || c.ContentVisibility != "visible") && c.Display != "inline" {
		c.OverflowX, c.OverflowY = "clip", "clip"
	}
	if c.OverflowX == "visible" && c.OverflowY != "visible" && c.OverflowY != "clip" {
		c.OverflowX = "auto"
	}
	if c.OverflowY == "visible" && c.OverflowX != "visible" && c.OverflowX != "clip" {
		c.OverflowY = "auto"
	}
	if c.OverflowX == "clip" && c.OverflowY != "clip" && c.OverflowY != "visible" {
		c.OverflowX = "hidden"
	}
	if c.OverflowY == "clip" && c.OverflowX != "clip" && c.OverflowX != "visible" {
		c.OverflowY = "hidden"
	}
	if c.Cursor == "" {
		c.Cursor = "auto"
	}
}

func blockify(d string) string {
	switch d {
	case "inline", "inline-block", "table-row-group", "table-header-group", "table-footer-group", "table-row", "table-cell",
		"table-column-group", "table-column", "table-caption", "ruby", "ruby-text":
		if d == "inline" || d == "inline-block" || strings.HasPrefix(d, "table-") || strings.HasPrefix(d, "ruby") {
			return "block"
		}
	case "inline-flex":
		return "flex"
	case "inline-grid":
		return "grid"
	case "inline-table":
		return "table"
	case "-webkit-inline-box":
		return "-webkit-box"
	}
	return d
}

// Blockify exposes display blockification for layout (flex/grid items).
func Blockify(d string) string { return blockify(d) }

// ---- global keywords -------------------------------------------------------------------

func isInheritedProp(name string) bool {
	switch name {
	case "color", "font", "font-family", "font-size", "font-weight", "font-style", "font-variant", "font-variant-caps", "font-stretch", "line-height",
		"letter-spacing", "word-spacing", "text-align", "text-align-last", "text-indent", "text-transform", "white-space", "word-break", "overflow-wrap",
		"word-wrap", "direction", "visibility", "cursor", "pointer-events", "list-style", "list-style-type", "list-style-position", "list-style-image",
		"quotes", "widows", "border-collapse", "border-spacing", "caption-side", "empty-cells", "tab-size", "text-shadow", "hyphens", "line-break", "text-wrap",
		"user-select", "writing-mode", "text-orientation", "color-scheme", "image-rendering", "text-rendering", "font-kerning", "-webkit-text-fill-color", "-webkit-text-stroke",
		"-webkit-text-stroke-width", "-webkit-text-stroke-color", "accent-color", "caret-color", "text-underline-offset", "orphans":
		return true
	}
	return false
}

func (s *cstate) global(name, kw string) {
	c, p := s.c, s.p
	if kw == "unset" || kw == "revert" || kw == "revert-layer" {
		if isInheritedProp(name) {
			kw = "inherit"
		} else {
			kw = "initial"
		}
	}
	if kw == "inherit" {
		if s.inheritProp(name) {
			return
		}
		if !isInheritedProp(name) {
			return
		}
		_ = p
		_ = c
		return
	}
	// initial
	if v, ok := initialValue(name); ok {
		s.apply(name, v, strings.ToLower(v))
	}
}

var initialVals = map[string]string{
	"display": "inline", "position": "static", "float": "none", "clear": "none", "width": "auto", "height": "auto", "min-width": "auto", "min-height": "auto",
	"max-width": "none", "max-height": "none", "margin": "0", "padding": "0", "top": "auto", "right": "auto", "bottom": "auto", "left": "auto", "inset": "auto",
	"z-index": "auto", "opacity": "1", "overflow": "visible", "overflow-x": "visible", "overflow-y": "visible", "vertical-align": "baseline",
	"text-decoration": "none", "text-decoration-line": "none", "background": "none transparent", "background-color": "transparent", "background-image": "none",
	"background-position": "0% 0%", "background-size": "auto", "background-repeat": "repeat", "background-attachment": "scroll", "background-clip": "border-box",
	"background-origin": "padding-box", "border": "medium none currentcolor", "border-width": "medium", "border-style": "none", "border-color": "currentcolor",
	"border-radius": "0", "box-shadow": "none", "transform": "none", "content": "normal", "flex": "0 1 auto", "flex-grow": "0", "flex-shrink": "1", "flex-basis": "auto",
	"flex-direction": "row", "flex-wrap": "nowrap", "order": "0", "align-items": "normal", "align-self": "auto", "align-content": "normal", "justify-content": "normal",
	"justify-items": "legacy", "justify-self": "auto", "gap": "normal", "row-gap": "normal", "column-gap": "normal", "box-sizing": "content-box", "outline": "medium none currentcolor",
	"color": "black", "font-family": "serif", "font-size": "medium", "font-weight": "normal", "font-style": "normal", "line-height": "normal", "letter-spacing": "normal",
	"word-spacing": "normal", "text-align": "start", "text-indent": "0", "text-transform": "none", "white-space": "normal", "word-break": "normal", "overflow-wrap": "normal",
	"visibility": "visible", "cursor": "auto", "list-style": "disc outside none", "list-style-type": "disc", "table-layout": "auto", "border-collapse": "separate",
	"text-shadow": "none", "filter": "none", "aspect-ratio": "auto", "object-fit": "fill", "pointer-events": "auto", "text-overflow": "clip",
	"grid-template-columns": "none", "grid-template-rows": "none", "grid-column": "auto", "grid-row": "auto", "grid-area": "auto", "grid-auto-flow": "row",
	"font-variant": "normal", "text-decoration-color": "currentcolor", "text-decoration-style": "solid", "mix-blend-mode": "normal", "transform-origin": "50% 50%", "perspective": "none", "perspective-origin": "50% 50%", "transform-style": "flat", "backface-visibility": "visible", "transform-box": "view-box",
	"translate": "none", "rotate": "none", "scale": "none",
}

func initialValue(name string) (string, bool) {
	if v, ok := initialVals[name]; ok {
		return v, true
	}
	if strings.HasPrefix(name, "margin-") || strings.HasPrefix(name, "padding-") {
		return "0", true
	}
	if strings.HasPrefix(name, "border-") {
		switch {
		case strings.HasSuffix(name, "-width"):
			return "medium", true
		case strings.HasSuffix(name, "-style"):
			return "none", true
		case strings.HasSuffix(name, "-color"):
			return "currentcolor", true
		case strings.HasSuffix(name, "-radius"):
			return "0", true
		}
		return "medium none currentcolor", true
	}
	return "", false
}

func (s *cstate) inheritProp(name string) bool {
	c, p := s.c, s.p
	switch name {
	case "color":
		c.Color = p.Color
	case "font":
		c.FontFamily, c.FontSize, c.FontWeight, c.FontStyle, c.SmallCaps, c.LineHeight = p.FontFamily, p.FontSize, p.FontWeight, p.FontStyle, p.SmallCaps, p.LineHeight
		s.setupMetrics()
	case "font-family":
		c.FontFamily = p.FontFamily
	case "font-size":
		c.FontSize, c.FontSizeKw, c.FontAbs = p.FontSize, p.FontSizeKw, p.FontAbs
		s.setupMetrics()
	case "font-weight":
		c.FontWeight = p.FontWeight
	case "font-style":
		c.FontStyle = p.FontStyle
	case "line-height":
		c.LineHeight = p.LineHeight
	case "letter-spacing":
		c.LetterSpacing = p.LetterSpacing
	case "word-spacing":
		c.WordSpacing = p.WordSpacing
	case "text-align":
		c.TextAlign = p.TextAlign
	case "text-indent":
		c.TextIndent = p.TextIndent
	case "text-transform":
		c.TextTransform = p.TextTransform
	case "white-space":
		c.WhiteSpace = p.WhiteSpace
	case "visibility":
		c.Visibility = p.Visibility
	case "cursor":
		c.Cursor = p.Cursor
	case "list-style", "list-style-type":
		c.ListStyleType = p.ListStyleType
		if name == "list-style" {
			c.ListStylePos, c.ListStyleImage = p.ListStylePos, p.ListStyleImage
		}
	case "display":
		c.Display = p.Display
	case "position":
		c.Position = p.Position
	case "float":
		c.Float = p.Float
	case "width":
		c.Width = p.Width
	case "height":
		c.Height = p.Height
	case "min-width":
		c.MinWidth = p.MinWidth
	case "max-width":
		c.MaxWidth = p.MaxWidth
	case "min-height":
		c.MinHeight = p.MinHeight
	case "max-height":
		c.MaxHeight = p.MaxHeight
	case "margin":
		c.Margin = p.Margin
	case "padding":
		c.Padding = p.Padding
	case "border":
		c.BorderWidth, c.BorderStyle, c.BorderColor, c.bcSet = p.BorderWidth, p.BorderStyle, p.BorderColor, p.bcSet
	case "border-color":
		c.BorderColor, c.bcSet = p.BorderColor, p.bcSet
	case "border-width":
		c.BorderWidth = p.BorderWidth
	case "border-style":
		c.BorderStyle = p.BorderStyle
	case "border-radius":
		c.Radius = p.Radius
	case "background", "background-color":
		c.BgColor = p.BgColor
		if name == "background" {
			c.BgLayers = p.BgLayers
		}
	case "background-image":
		c.BgLayers = p.BgLayers
	case "text-decoration":
		c.TextDecorLine, c.TextDecorStyle, c.TextDecorColor, c.DecorCur = p.TextDecorLine, p.TextDecorStyle, p.TextDecorColor, p.DecorCur
	case "opacity":
		c.Opacity, c.OpacitySet = p.Opacity, p.OpacitySet
	case "overflow":
		c.OverflowX, c.OverflowY = p.OverflowX, p.OverflowY
	case "vertical-align":
		c.VerticalAlign, c.VerticalAlignLen = p.VerticalAlign, p.VerticalAlignLen
	case "box-sizing":
		c.BoxSizing = p.BoxSizing
	case "box-shadow":
		c.BoxShadow = p.BoxShadow
	case "transform":
		c.Transform = p.Transform
	case "flex":
		c.FlexGrow, c.FlexShrink, c.FlexBasis = p.FlexGrow, p.FlexShrink, p.FlexBasis
	case "align-items":
		c.AlignItems = p.AlignItems
	case "justify-content":
		c.JustifyContent = p.JustifyContent
	case "gap":
		c.RowGap, c.ColumnGap = p.RowGap, p.ColumnGap
	case "z-index":
		c.ZIndex, c.ZAuto = p.ZIndex, p.ZAuto
	case "content":
		c.Content, c.ContentSet, c.ContentNone = p.Content, p.ContentSet, p.ContentNone
	default:
		return false
	}
	return true
}

// Anonymous returns the style of an anonymous box below parent.
func (e *Engine) Anonymous(parent *Computed, display string) *Computed {
	c := e.newComputed(parent)
	c.Display = display
	e.finalize(c, parent, "")
	return c
}

// Initial returns the engine's initial-values style (parent of the root).
func (e *Engine) InitialStyle() *Computed { return e.initial }

// quirkMarginTag lists the elements whose UA margins are "quirky" (Blink's
// 1__qem): ignored at the start/end of body and table cells in quirks mode.
func quirkMarginTag(tag string) bool {
	switch tag {
	case "p", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "menu", "dir", "dl", "blockquote", "pre", "listing", "xmp", "plaintext":
		return true
	}
	return false
}

// EntryIndex buckets the entries of a rule list by the id, class or tag of
// their rightmost compound selector, like the Engine does, so that code
// running its own cascade (the SVG scene builder) only matches the selectors
// that can apply to a node instead of every rule of the page.
type EntryIndex struct {
	byID, byClass, byTag map[string][]*Entry
	univ                 []*Entry
}

// NewEntryIndex indexes the entries of rules.
func NewEntryIndex(rules []*Rule) *EntryIndex {
	ix := &EntryIndex{byID: map[string][]*Entry{}, byClass: map[string][]*Entry{}, byTag: map[string][]*Entry{}}
	for _, r := range rules {
		for _, en := range r.Entries {
			k, v := bucketKey(en.key)
			switch k {
			case 'i':
				ix.byID[v] = append(ix.byID[v], en)
			case 'c':
				ix.byClass[v] = append(ix.byClass[v], en)
			case 't':
				ix.byTag[v] = append(ix.byTag[v], en)
			default:
				ix.univ = append(ix.univ, en)
			}
		}
	}
	return ix
}

// Candidates appends to out every entry that can match n (a superset: the
// caller still runs the selector).
func (ix *EntryIndex) Candidates(n *html.Node, out []*Entry) []*Entry {
	if id, ok := attrOf(n, "id"); ok && id != "" {
		out = append(out, ix.byID[id]...)
	}
	if cl, ok := attrOf(n, "class"); ok && cl != "" {
		for _, c := range strings.Fields(cl) {
			out = append(out, ix.byClass[c]...)
		}
	}
	out = append(out, ix.byTag[n.Data]...)
	if lc := strings.ToLower(n.Data); lc != n.Data {
		out = append(out, ix.byTag[lc]...)
	}
	return append(out, ix.univ...)
}
