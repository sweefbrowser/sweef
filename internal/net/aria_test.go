package net

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden .aria.yaml files")

// TestGoldenSnapshots compares snapshot v2 output for saved real pages against
// checked-in golden files. Run `go test ./internal/net -update` after an
// intentional format change and review the diff.
func TestGoldenSnapshots(t *testing.T) {
	pages, _ := filepath.Glob("testdata/*.html")
	if len(pages) == 0 {
		t.Fatal("no testdata pages")
	}
	for _, p := range pages {
		name := strings.TrimSuffix(filepath.Base(p), ".html")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			d, err := ParseDocument(string(src), "https://"+name+".test/")
			if err != nil {
				t.Fatal(err)
			}
			got, err := d.AriaSnapshotYAML()
			if err != nil {
				t.Fatal(err)
			}
			golden := strings.TrimSuffix(p, ".html") + ".aria.yaml"
			if *update {
				os.WriteFile(golden, []byte(got), 0o644)
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden %s (run with -update)", golden)
			}
			if string(want) != got {
				t.Errorf("snapshot for %s differs from golden; run with -update and review", name)
			}
		})
	}
}

func TestSnapshotContentAndRefs(t *testing.T) {
	src, _ := os.ReadFile("testdata/example_com.html")
	d, _ := ParseDocument(string(src), "https://example.com/")
	y, err := d.AriaSnapshotYAML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(y, "- paragraph: This domain is for use") {
		t.Fatalf("paragraph missing:\n%s", y)
	}
	if strings.Contains(y, "ref=e") {
		t.Fatalf("plain content must not carry refs:\n%s", y)
	}
}

func TestSnapshotGitHubLogin(t *testing.T) {
	src, _ := os.ReadFile("testdata/github_login.html")
	d, _ := ParseDocument(string(src), "https://github.com/login")
	y, err := d.AriaSnapshotYAML()
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(y, "- textbox"); n != 2 {
		t.Fatalf("want 2 textboxes (no hidden inputs), got %d:\n%s", n, y)
	}
	if strings.Contains(y, "authenticity_token") || strings.Contains(y, "timestamp") {
		t.Fatalf("hidden input values leaked:\n%s", y)
	}
	if !strings.Contains(y, `textbox "Username or email address" [required ref=e`) {
		t.Fatalf("label-for name missing:\n%s", y)
	}
	if !strings.Contains(y, `- button "Sign in" [ref=e`) {
		t.Fatalf("sign in button missing:\n%s", y)
	}
}

func TestSnapshotRefsResolveAndSurviveMutation(t *testing.T) {
	src := `<html><body><ul id="l"><li><a href="/a">A</a></li></ul><button id="b">Go</button></body></html>`
	d, _ := ParseDocument(src, "https://t.test/")
	y, _ := d.AriaSnapshotYAML()
	i := strings.Index(y, `button "Go" [ref=e`)
	if i < 0 {
		t.Fatal(y)
	}
	ref := y[i+len(`button "Go" [ref=`):]
	ref = ref[:strings.IndexByte(ref, ']')]
	infos, err := d.ResolveEngine(ref)
	if err != nil || infos[0].ID != "b" {
		t.Fatalf("ref %s -> %v %v", ref, infos, err)
	}
	// mutate the tree before the button: refs must not move
	if err := d.AppendHTML("#l", "<li><a href='/x'>X</a></li><li><a href='/y'>Y</a></li>"); err != nil {
		t.Fatal(err)
	}
	infos, err = d.ResolveEngine(ref)
	if err != nil || infos[0].ID != "b" {
		t.Fatalf("after mutation ref %s -> %v %v", ref, infos, err)
	}
	// detached and unknown refs give distinct, actionable errors
	if err := d.RemoveNode("#b"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ResolveEngine(ref); err == nil || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("want detached error, got %v", err)
	}
	if _, err := d.ResolveEngine("e9999"); err == nil || !strings.Contains(err.Error(), "not in any snapshot") {
		t.Fatalf("want unknown-ref error, got %v", err)
	}
}

func TestSnapshotOptions(t *testing.T) {
	src, _ := os.ReadFile("testdata/wikipedia_go.html")
	d, _ := ParseDocument(string(src), "https://en.wikipedia.org/")
	full, _ := d.AriaSnapshotV2(SnapshotOptions{MaxChars: 1 << 20})
	capped, _ := d.AriaSnapshotV2(SnapshotOptions{MaxChars: 20000})
	if len(capped) > 20200 || !strings.Contains(capped, "truncated") {
		t.Fatalf("cap not honoured: %d bytes", len(capped))
	}
	inter, _ := d.AriaSnapshotV2(SnapshotOptions{InteractiveOnly: true, MaxChars: 1 << 20})
	if strings.Contains(inter, "- text:") || strings.Contains(inter, "- paragraph: ") {
		t.Fatal("interactiveOnly leaked content")
	}
	if len(inter) >= len(full) {
		t.Fatalf("interactiveOnly should be much smaller: %d vs %d", len(inter), len(full))
	}
	scoped, err := d.AriaSnapshotV2(SnapshotOptions{Selector: "#mw-content-text"})
	if err != nil || !strings.Contains(scoped, "paragraph") {
		t.Fatalf("scoped: %v %d", err, len(scoped))
	}
	t.Logf("wikipedia: full=%d capped=%d interactive=%d scoped=%d", len(full), len(capped), len(inter), len(scoped))
}

func TestSnapshotDefaultCapAndOutline(t *testing.T) {
	src, _ := os.ReadFile("testdata/wikipedia_go.html")
	d, _ := ParseDocument(string(src), "https://en.wikipedia.org/")
	got, err := d.AriaSnapshotV2(SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 20000 {
		t.Fatalf("default snapshot is %d bytes, want at most 20000", len(got))
	}
	i := strings.Index(got, "- outline (whole page): ")
	if i < 0 || !strings.Contains(got[i:], "History") {
		t.Fatalf("a truncated snapshot must end with an outline of the whole page, got tail %q", got[max(0, len(got)-300):])
	}
	small, _ := ParseDocument("<h1>Hi</h1><button>Go</button>", "https://x.test/")
	s, _ := small.AriaSnapshotV2(SnapshotOptions{})
	if strings.Contains(s, "outline") || strings.Contains(s, "truncated") {
		t.Fatalf("a snapshot that fits carries no footer: %q", s)
	}
}
