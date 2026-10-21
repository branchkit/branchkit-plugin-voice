package views

import (
	"context"
	"strings"
	"testing"
)

func renderModelStatus(t *testing.T, ready bool, dl ModelDownloadView, ref string) string {
	t.Helper()
	var b strings.Builder
	if err := modelStatus(ready, dl, ref, "2.5 GB", false).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// A Download button spins (the platform's aria-busy look) while its own
// request is in flight, and each button has a flag of its own, so clicking
// one never spins the other.
func TestDownloadButtonIsBusyWhileItsRequestRuns(t *testing.T) {
	a := renderModelStatus(t, false, ModelDownloadView{}, "voice/qwen3-4b")
	b := renderModelStatus(t, false, ModelDownloadView{}, "voice/qwen2.5-1.5b-instruct")
	for _, want := range []string{`data-indicator:busyvoiceqwen34b`, `data-attr:aria-busy="$busyvoiceqwen34b"`} {
		if !strings.Contains(a, want) {
			t.Errorf("want %s on the button:\n%s", want, a)
		}
	}
	if !strings.Contains(b, `data-indicator:busyvoiceqwen2515binstruct`) {
		t.Errorf("the other model's button needs its own flag:\n%s", b)
	}
}

// A download that stopped short says why beside its button, and the button
// stays so the person can try again.
func TestFailedDownloadSaysWhy(t *testing.T) {
	html := renderModelStatus(t, false, ModelDownloadView{Failed: "checksum mismatch"}, "voice/qwen3-4b")
	if !strings.Contains(html, "Download failed: checksum mismatch") || !strings.Contains(html, ">Download</button>") {
		t.Errorf("a failure must name its reason and keep the button:\n%s", html)
	}
}

// Between the click and the first reported percent, the bar sweeps and says
// it is starting, instead of sitting still at 0%. Once a percent arrives the
// bar shows it.
func TestDownloadSweepsUntilTheFirstPercent(t *testing.T) {
	start := renderModelStatus(t, false, ModelDownloadView{Active: true, Pct: 0}, "voice/qwen3-4b")
	if !strings.Contains(start, "bk-progress-indeterminate") || !strings.Contains(start, "Starting download") {
		t.Errorf("0%% must sweep and say it is starting:\n%s", start)
	}
	mid := renderModelStatus(t, false, ModelDownloadView{Active: true, Pct: 42}, "voice/qwen3-4b")
	if strings.Contains(mid, "bk-progress-indeterminate") || !strings.Contains(mid, "Downloading model · 42%") {
		t.Errorf("42%% must show the percent, not the sweep:\n%s", mid)
	}
}
