package breakout

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Deployment-gate fixes for the 2026-10-06 review (P2-11): runOnce's recover
// must reset `scanning` and fire onDone exactly once; the AnalyzeMany and
// ScanShorts worker goroutines must carry their own recover (the runOnce
// recover lives on the caller's goroutine and cannot catch a worker panic —
// independently reproduced pre-fix as exit status 2).

type fix06Transport func(*http.Request) (*http.Response, error)

func (f fix06Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fix06Response(body string) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

// Parent goroutine: after a recovered panic the board must not wedge —
// scanning cleared, onDone fired exactly once, RefreshNow free to scan again.
func TestFix06RunOncePanicClearsScanningAndFiresOnDone(t *testing.T) {
	old := binanceHTTP
	defer func() { binanceHTTP = old }()
	calls := 0
	binanceHTTP = &http.Client{Transport: fix06Transport(func(*http.Request) (*http.Response, error) {
		calls++
		panic("fix06 parent panic")
	})}
	s := &Scheduler{scanning: true, Symbols: 1}
	done := 0
	sawScanning := true
	s.runOnce(func() { done++; sawScanning = s.scanning })
	if s.scanning {
		t.Fatal("recovered panic left scanning=true — the board stays wedged until restart")
	}
	if done != 1 {
		t.Fatalf("onDone fired %d times after a panic, want exactly 1", done)
	}
	if sawScanning {
		t.Fatal("panic path: onDone observed scanning=true — callback ran before the cleanup defer")
	}
	// The next synchronous refresh must complete without wedging on a stale
	// flag (pre-fix: RefreshNow spun to its deadline while scanning stayed
	// true). The contract is no-deadlock; scan success is irrelevant here.
	s.RefreshNow(2 * time.Second)
	if s.scanning {
		t.Fatal("RefreshNow left scanning=true after the panic recovery")
	}
}

// onDone must fire exactly once on the NORMAL path too — and only AFTER
// scanning is cleared (fix-recheck 2026-10-06: the body used to fire the
// callback before the defer reset the flag).
func TestFix06RunOnceNormalPathFiresOnDoneOnce(t *testing.T) {
	old := binanceHTTP
	defer func() { binanceHTTP = old }()
	binanceHTTP = &http.Client{Transport: fix06Transport(func(*http.Request) (*http.Response, error) {
		return fix06Response(`[]`)
	})}
	s := &Scheduler{scanning: true, Symbols: 1}
	done := 0
	sawScanning := true
	s.runOnce(func() { done++; sawScanning = s.scanning })
	if done != 1 || s.scanning {
		t.Fatalf("normal path: done=%d scanning=%v, want 1/false", done, s.scanning)
	}
	if sawScanning {
		t.Fatal("normal path: onDone observed scanning=true — callback ran before the cleanup defer")
	}
}

// The ERROR path (top-volume listing fails) must behave identically: callback
// fires exactly once, and only after scanning is cleared.
func TestFix06RunOnceErrorPathOnDoneAfterCleanup(t *testing.T) {
	old := binanceHTTP
	defer func() { binanceHTTP = old }()
	binanceHTTP = &http.Client{Transport: fix06Transport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	s := &Scheduler{scanning: true, Symbols: 1}
	called := 0
	sawScanning := true
	s.runOnce(func() { called++; sawScanning = s.scanning })
	if called != 1 {
		t.Fatalf("error path: onDone fired %d times, want exactly 1", called)
	}
	if sawScanning {
		t.Fatal("error path: onDone observed scanning=true — callback ran before the cleanup defer")
	}
	if s.scanning {
		t.Fatal("error path left scanning=true")
	}
}

func fix06WorkerMode(mode string) {
	setGainerHistoryPath(filepath.Join(os.TempDir(), fmt.Sprintf("fix06-gainers-%d.json", os.Getpid())))
	tickerOK := `[{"symbol":"XUSDT","lastPrice":"100","priceChangePercent":"10","quoteVolume":"20000000","count":100}]`
	binanceHTTP = &http.Client{Transport: fix06Transport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/ticker/24hr") {
			return fix06Response(tickerOK)
		}
		if r.URL.Query().Get("symbol") == "XUSDT" {
			panic("fix06 worker panic " + mode)
		}
		return fix06Response(`[]`)
	})}
	if mode == "analyze" {
		(&Scheduler{scanning: true, Symbols: 1, Concurrent: 1}).runOnce(nil)
		return
	}
	// shorts mode: a ScanShorts worker panic must be recovered by ITS OWN
	// defer (analyze mode also exercises ScanShorts via runOnce's tail; the
	// standalone call keeps the coverage independent of the scheduler).
	_, _, _ = ScanShorts(ShortScanUniverse, -1, -1)
}

// Worker panics run in child processes (a worker panic pre-fix killed the
// test process). PASS here means the child SURVIVED — i.e. the per-worker
// recover works. If the outer recover marker prints, the fix regressed.
func TestFix06WorkerPanicsAreRecovered(t *testing.T) {
	if mode := os.Getenv("NOFX_FIX06_WORKER"); mode != "" {
		fix06WorkerMode(mode)
		fmt.Println("WORKER_SURVIVED_" + strings.ToUpper(mode))
		return
	}
	for _, mode := range []string{"analyze", "shorts"} {
		cmd := exec.Command(os.Args[0], "-test.run", "TestFix06WorkerPanicsAreRecovered")
		cmd.Env = append(os.Environ(), "NOFX_FIX06_WORKER="+mode)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("worker mode %s: child process died (panic escaped the per-worker recover): %v\n%s", mode, err, out)
		}
		if !strings.Contains(string(out), "WORKER_SURVIVED_"+strings.ToUpper(mode)) {
			t.Fatalf("worker mode %s: child exited without reaching the marker (outer recover caught it instead?):\n%s", mode, out)
		}
		if strings.Contains(string(out), "OUTER_RECOVERED") {
			t.Fatalf("worker mode %s: panic escaped the worker and was caught by the OUTER recover:\n%s", mode, out)
		}
	}
}
