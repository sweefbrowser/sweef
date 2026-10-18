package style

import (
	"strings"
	"testing"
)

// HasGeneratedContent looks only at the rules whose id, class or tag bucket the element can hit. It must agree
// with a scan of every content rule, in standards and quirks mode (class and id fold case there).
func TestHasGeneratedContentBuckets(t *testing.T) {
	src := `<html><head><style>
#Hero::before { content: "a" }
.Icon::after { content: "" }
li::before { content: "-" }
*::after { content: none }
.plain::before { color: red }
.x .y::before { content: "q" }
</style></head><body><div id=hero class=icon><ul><li class="a y">k</li></ul><p class=plain>t</p><div class=x><span class=y>s</span></div></div></body></html>`
	type want struct {
		tag, pseudo string
		std, quirks bool
	}
	for _, w := range []want{
		{"div", "before", false, true}, // #Hero only matches the lowercase id in quirks mode
		{"div", "after", true, true},   // *::after declares content (none counts as a declaration; the universal bucket)
		{"li", "before", true, true},
		{"p", "before", false, false}, // .plain::before declares no content
		{"span", "before", true, true},
		{"ul", "before", false, false},
	} {
		for _, quirks := range []bool{false, true} {
			doc := parse(t, src)
			e := NewEngineQ(&Env{Width: 1280, Height: 800}, []SheetIn{{Text: strings.Join(extractStyles(doc), "\n")}}, quirks)
			e.BeginPass()
			n := find(doc, w.tag)
			// the reference: every content rule against the element
			ref := false
			for _, en := range e.rules {
				for _, x := range en.Entries {
					if x.Pseudo == w.pseudo && declaresContent(en.Decls) && e.entryMatches(n, x, w.pseudo) {
						ref = true
					}
				}
			}
			got := e.HasGeneratedContent(n, w.pseudo)
			exp := w.std
			if quirks {
				exp = w.quirks
			}
			if got != ref || got != exp {
				t.Errorf("<%s>::%s quirks=%v: HasGeneratedContent=%v, full scan=%v, want %v", w.tag, w.pseudo, quirks, got, ref, exp)
			}
		}
	}
}
