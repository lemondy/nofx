package binance

import (
	"strings"
	"testing"
)

// getBrOrderIDFor must keep the broker referral prefix (revival attribution)
// and stay inside Binance's 32-char client-ID cap, with the lim- class marker
// surviving truncation of the timestamp tail.
func TestGetBrOrderIDFor(t *testing.T) {
	bare := getBrOrderID()
	if !strings.HasPrefix(bare, "x-KzrpZaP9") {
		t.Fatalf("bare order ID lost broker prefix: %q", bare)
	}

	tagged := getBrOrderIDFor("lim-1a2b-1788966018463")
	if !strings.HasPrefix(tagged, "x-KzrpZaP9") {
		t.Errorf("tagged order ID must keep broker prefix, got %q", tagged)
	}
	if !strings.Contains(tagged, "lim-1a2b") {
		t.Errorf("tagged order ID must carry the lim- tag, got %q", tagged)
	}
	if len(tagged) > 32 {
		t.Errorf("client ID exceeds Binance's 32-char cap: %d", len(tagged))
	}

	long := getBrOrderIDFor("lim-1a2b-17889660184639999999999999")
	if !strings.HasPrefix(long, "x-KzrpZaP9lim-1a2b") {
		t.Errorf("right-truncation must keep the lim-<hash> head, got %q", long)
	}
	if len(long) > 32 {
		t.Errorf("truncated client ID exceeds cap: %d", len(long))
	}
}
