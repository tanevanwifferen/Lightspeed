package cli

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/index"
)

// TestIndexPerformanceAgainstThisRepo measures the index with a real gopls on
// this repository, through the real command path and a real daemon:
//
//	cold `index build`                       well under 30 s
//	warm `search_symbols`, end to end        under 100 ms
//	revalidation of an unchanged tree        under 150 ms
//
// It is skipped, cleanly, when gopls is not usable on this machine, under
// -short, and with LIGHTSPEED_SKIP_PERF=1: nothing else in the suite needs a
// real language server, and this one is a measurement, not a proof of behaviour.
// Run it with -v to see the numbers.
func TestIndexPerformanceAgainstThisRepo(t *testing.T) {
	if testing.Short() || os.Getenv("LIGHTSPEED_SKIP_PERF") == "1" {
		t.Skip("performance measurement skipped")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("not running inside the lightspeed repository")
	}
	t.Chdir(root)
	daemonEnv(t, "120s")
	t.Setenv(fakeServerModeEnv, "")

	t0 := time.Now()
	code, out, stderr := runMain("index", "build", "--timeout", "120s")
	cold := time.Since(t0)
	if code != ExitOK {
		t.Skipf("index build did not succeed (gopls not usable here?): exit %d\n%s%s", code, out, stderr)
	}
	var br index.BuildResult
	decodeData(t, out, &br)
	goServed := 0
	for _, s := range br.Status.Servers {
		if s.Name == "gopls" {
			goServed = s.Indexed
		}
	}
	if goServed == 0 {
		t.Skipf("gopls is not usable on this machine, so nothing was outlined: %+v", br.Status.Servers)
	}
	t.Logf("cold index build: %s wall (%s in the index) — %d files, %d outlines, %d symbols, %d imports",
		cold.Round(time.Millisecond), br.Report.Elapsed.Round(time.Millisecond), br.Report.Built, br.Report.Outlines, br.Status.Symbols, br.Status.Imports)
	if cold > 30*time.Second {
		t.Errorf("cold build took %s, want well under 30s", cold)
	}

	measure := func(what string, n int, args ...string) (median, worst time.Duration) {
		t.Helper()
		var ds []time.Duration
		for i := 0; i < n; i++ {
			start := time.Now()
			code, out, stderr := runMain(args...)
			d := time.Since(start)
			if code != ExitOK {
				t.Fatalf("%s: exit %d\n%s%s", what, code, out, stderr)
			}
			ds = append(ds, d)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[len(ds)/2], ds[len(ds)-1]
	}

	// One warm-up of each, then the measurement.
	runMain("search_symbols", "Manager")
	med, worst := measure("search_symbols", 25, "search_symbols", "Manager")
	t.Logf("warm search_symbols: median %s, worst %s over 25 runs", med.Round(time.Microsecond), worst.Round(time.Microsecond))
	if med > 100*time.Millisecond {
		t.Errorf("warm search_symbols median %s, want under 100ms", med)
	}

	runMain("index", "build")
	med, worst = measure("index build (unchanged)", 15, "index", "build")
	t.Logf("revalidation of the unchanged repo (`index build`): median %s, worst %s over 15 runs", med.Round(time.Microsecond), worst.Round(time.Microsecond))
	if med > 150*time.Millisecond {
		t.Errorf("revalidating an unchanged repo took a median of %s, want under 150ms", med)
	}
	code, out, _ = runMain("index", "build")
	br = index.BuildResult{}
	decodeData(t, out, &br)
	if code != ExitOK || br.Report.Built != 0 || br.Report.Touched != 0 {
		t.Errorf("an unchanged repo rebuilt something: %+v", br.Report)
	}
	t.Logf("in-index revalidation: scan %s, total %s, listed=%v", br.Report.Scan.Round(time.Microsecond), br.Report.Elapsed.Round(time.Microsecond), br.Report.Listed)
}
