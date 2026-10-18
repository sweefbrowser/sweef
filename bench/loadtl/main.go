// Command loadtl prints the per-phase timeline of a page load through the
// engine: document request (DNS, connect, TLS, first byte), parse, stylesheet
// and script fetches, script execution, lifecycle events, layout (with the
// image fetches it triggers) and the first paint. It uses the same engine
// options as the GUI (300 ms settle budget) and honours HTTPS_PROXY.
//
//	go run ./bench/loadtl -url https://go.dev/doc/effective_go
//	go run ./bench/loadtl -sites bench/datasaver/sites.txt -n 5 -summary
//	go run ./bench/loadtl -url ... -progressive      (the GUI's loading path)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/sweefbrowser/sweef/internal/engine"
)

func main() {
	var (
		one     = flag.String("url", "", "load this page")
		sites   = flag.String("sites", "", "file with one URL per line (bench/datasaver/sites.txt format)")
		n       = flag.Int("n", 5, "with -sites: how many pages")
		settle  = flag.Int("settle-ms", 300, "engine settle budget")
		prog    = flag.Bool("progressive", false, "use the GUI's progressive loading path")
		full    = flag.Bool("full", false, "print every mark, not only the long ones")
		summary = flag.Bool("summary", false, "one line per page")
		blockTr = flag.Bool("block-trackers", true, "block known trackers and analytics, as the CLI, MCP and agent do")
		cache   = flag.String("cache", "off", "cache dir (off = none; a path keeps the bytecode and HTTP cache between runs)")
		twice   = flag.Bool("twice", false, "load every page twice with the same cache dir (second visit)")
		console = flag.Bool("console", false, "print the page's console messages (script errors) after the load")
		agent   = flag.Bool("agent", false, "use the CLI/MCP daemon's load settings (nav settle 600/150 ms, quiet 60 ms, 2 warm VMs) instead of the library defaults")
		minMs   = flag.Float64("min-ms", 25, "hide spans shorter than this unless -full")
	)
	flag.IntVar(&consoleMax, "console-width", 400, "with -console: characters of each message to print")
	prof := flag.String("cpuprofile", "", "write a CPU profile of the load here")
	flag.StringVar(&memProf, "memprofile", "", "write a heap profile after the load (and -run-ms) here")
	flag.StringVar(&initJS, "init", "", "file with JavaScript to run before the page's scripts (a debugging hook)")
	flag.StringVar(&evalAfter, "eval", "", "JavaScript expression to evaluate and print after the load")
	flag.IntVar(&waitAfter, "run-ms", 0, "with -eval: run the page this long after the load first")
	flag.Parse()
	if *prof != "" {
		f, err := os.Create(*prof)
		if err == nil {
			pprof.StartCPUProfile(f)
			defer pprof.StopCPUProfile()
		}
	}
	var urls []string
	if *one != "" {
		urls = append(urls, *one)
	}
	if *sites != "" {
		f, err := os.Open(*sites)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() && len(urls) < *n {
			l := strings.TrimSpace(sc.Text())
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			urls = append(urls, strings.TrimSpace(strings.SplitN(l, "|", 2)[0]))
		}
		f.Close()
	}
	if len(urls) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	o := engine.DefaultOptions()
	o.ViewportWidth, o.ViewportHeight = 1280, 760
	o.SettleMs = *settle
	o.BlockTrackers = *blockTr
	if *agent {
		o.NavSettleMs, o.NavPostLoadMs, o.SettleQuietMs, o.NavQuietMs = 600, 150, 60, 60
		o.HideScrollbars, o.NoAutoplay, o.WarmVMs = true, true, 2
	}
	o.CacheDir = *cache
	if *cache != "off" && *cache != "" {
		o.DiskCacheMiB = 50
	}
	e := engine.New(o)
	defer e.Close()
	for _, u := range urls {
		for pass := 0; pass < 1+b2i(*twice); pass++ {
			run(e, u, *prog, *full, *summary, *console, *minMs, pass)
		}
	}
}

type prog struct {
	ready, firstFrame, loaded, done, lastPaint time.Duration
	frames                                     int
}

var consoleMax = 400

var initJS, evalAfter, memProf string
var waitAfter int

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func run(e *engine.Engine, u string, progressive, full, summary, console bool, minMs float64, pass int) {
	t, err := e.NewTab("about:blank")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer e.CloseTab(t.ID)
	t.TraceNetwork(true)
	if initJS != "" {
		src, err := os.ReadFile(initJS)
		if err != nil {
			fmt.Println(err)
			return
		}
		t.AddInitScript(string(src))
	}
	t0 := time.Now()
	var pr prog
	if progressive {
		err = loadProgressive(t, u, &pr, t0)
	} else {
		err = t.Navigate(u, nil)
	}
	navDone := time.Since(t0)
	if evalAfter != "" && err == nil {
		if waitAfter > 0 {
			t.RunFor(time.Duration(waitAfter) * time.Millisecond)
		}
		r, eerr := t.Evaluate(evalAfter)
		fmt.Printf("eval: %s %v\n", r, eerr)
	}
	if memProf != "" {
		runtime.GC()
		if f, err := os.Create(memProf); err == nil {
			_ = pprof.WriteHeapProfile(f)
			f.Close()
		}
	}
	if err != nil {
		fmt.Printf("%s: %v\n", u, err)
		return
	}
	var paint time.Duration
	if !progressive {
		tp := time.Now()
		t.PaintRGBA(1280, 760*3, 1, false)
		paint = time.Since(tp)
	}
	visit := ""
	if pass > 0 {
		visit = " (2nd visit)"
	}
	if progressive {
		fmt.Printf("%-52s ready %5.0f  FIRST FRAME %5.0f ms  load %5.0f  done %5.0f  last image repaint %5.0f  (%d frames)%s\n", short(u), ms(pr.ready), ms(pr.firstFrame), ms(pr.loaded), ms(pr.done), ms(pr.lastPaint), pr.frames, visit)
	} else {
		fmt.Printf("%-60s navigate %6.0f ms   +first paint %5.0f ms = %6.0f ms%s\n", short(u), ms(navDone), ms(paint), ms(navDone+paint), visit)
	}
	if console {
		for _, ev := range t.ConsoleLog(0) {
			if d, ok := ev.Data.(map[string]string); ok {
				fmt.Printf("   console.%s: %s\n", d["level"], trunc(strings.ReplaceAll(d["text"], "\n", " | "), consoleMax))
			}
		}
	}
	if summary {
		return
	}
	marks := t.LoadTimeline()
	sort.SliceStable(marks, func(i, j int) bool { return marks[i].At < marks[j].At })
	var imgN int
	var imgSum time.Duration
	for _, m := range marks {
		if m.Name == "img.fetch" {
			imgN++
			imgSum += m.Dur
		}
		if !full && m.Dur < time.Duration(minMs*float64(time.Millisecond)) && m.Dur != 0 {
			continue
		}
		if !full && m.Dur == 0 && !strings.HasPrefix(m.Name, "event.") && !strings.HasPrefix(m.Name, "scripts.") && !strings.HasPrefix(m.Name, "first") && !strings.HasPrefix(m.Name, "load") {
			continue
		}
		fmt.Printf("   %7.0f  %7.0f  %-22s %s\n", ms(m.At), ms(m.Dur), m.Name, trunc(m.Detail, 90))
	}
	fmt.Printf("   (images fetched during layout: %d, summed %.0f ms)\n\n", imgN, ms(imgSum))
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func short(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return trunc(u, 60)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
