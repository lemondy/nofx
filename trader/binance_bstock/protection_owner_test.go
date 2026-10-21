package binance_bstock

import (
	"strings"
	"testing"

	binance "github.com/adshao/go-binance/v2"
)

func ownedStop(t *testing.T, tr *BStockTrader, orderID int64) *binance.Order {
	t.Helper()
	o := stopOrder(-1)
	o.OrderID = orderID
	o.ClientOrderID = tr.protectionID("sl")
	return o
}

func deletes(f *fakeTransport) int {
	n := 0
	for _, r := range f.requests {
		if r.method == "DELETE" {
			n++
		}
	}
	return n
}

func TestProtectionIDCarriesOwnerTag(t *testing.T) {
	tr, _, _ := fixture(t)
	if tr.protectionID("sl") == tr.protectionID("sl") {
		t.Fatal("IDs must be unique")
	}
	tr.SetProtectionOwner(OwnerTag("trader-A"), nil)
	id := tr.protectionID("tp")
	tagged(t, id)
	if got := protectionTag(id); got != OwnerTag("trader-A") || len(got) != ownerTagLen {
		t.Fatalf("tag %q in %q", got, id)
	}
	if !strings.HasPrefix(id, "nxbs_tp_") {
		t.Fatalf("role prefix lost: %q", id)
	}
	if protectionTag("nxbs_sl_0123456789abcdef01234567") != "" {
		t.Fatal("a legacy ID must not parse as tagged")
	}
	if OwnerTag("trader-A") == OwnerTag("trader-B") {
		t.Fatal("tags of different traders collide")
	}
}

// Trader B sharing the account must not see, cancel or replace trader A's stop.
func TestOtherTradersProtectionIsNeverTouched(t *testing.T) {
	trA, _, _ := fixture(t)
	trA.SetProtectionOwner(OwnerTag("A"), nil)
	theirs := ownedStop(t, trA, 700)

	tr, f, _ := fixture(t)
	tr.SetProtectionOwner(OwnerTag("B"), nil)
	f.orders = []*binance.Order{theirs}
	if p, err := tr.GetProtection("AAPLBUSDT"); err != nil || p != nil {
		t.Fatalf("B must not report A's protection: %+v %v", p, err)
	}
	if err := tr.CancelProtection("AAPLBUSDT"); err != nil || deletes(f) != 0 {
		t.Fatalf("B cancelled A's protection: err=%v requests=%+v", err, f.requests)
	}
	if err := tr.CancelAllOrders("AAPLBUSDT"); err != nil || deletes(f) != 0 {
		t.Fatalf("B's CancelAllOrders hit A's protection: err=%v requests=%+v", err, f.requests)
	}
	p, err := tr.SetProtection("AAPLBUSDT", 2, 90, 89.5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deletes(f) != 0 {
		t.Fatalf("B's new protection cancelled A's: %+v", f.requests)
	}
	if protectionTag(p.StopClientID) != OwnerTag("B") {
		t.Fatalf("new leg not tagged for B: %q", p.StopClientID)
	}
	// Once B's own leg is open it is B's protection; A's stays invisible.
	own := ownedStop(t, tr, 701)
	f.orders = []*binance.Order{theirs, own}
	got, err := tr.GetProtection("AAPLBUSDT")
	if err != nil || got == nil || got.StopOrderID != "701" {
		t.Fatalf("B's own protection: %+v %v", got, err)
	}
}

// Pre-upgrade (untagged) legs belong to a trader only when its registry claims
// the immutable order ID; a shared prefix alone is not ownership.
func TestLegacyProtectionNeedsRegistryOwnership(t *testing.T) {
	legacy := stopOrder(-1) // nxbs_sl_old, order 10
	for _, tc := range []struct {
		name  string
		owned func(symbol, orderID string) bool
		want  bool
	}{
		{"unclaimed", func(string, string) bool { return false }, false},
		{"no registry", nil, false},
		{"claimed", func(sym, id string) bool { return sym == "AAPLBUSDT" && id == "10" }, true},
		{"other order claimed", func(string, string) bool { return false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, f, _ := fixture(t)
			tr.SetProtectionOwner(OwnerTag("B"), tc.owned)
			f.orders = []*binance.Order{legacy}
			p, err := tr.GetProtection("AAPLBUSDT")
			if err != nil {
				t.Fatal(err)
			}
			if (p != nil) != tc.want {
				t.Fatalf("GetProtection=%+v, want owned=%v", p, tc.want)
			}
			if err := tr.CancelProtection("AAPLBUSDT"); err != nil {
				t.Fatal(err)
			}
			if (deletes(f) == 1) != tc.want {
				t.Fatalf("cancel requests %d, want cancelled=%v", deletes(f), tc.want)
			}
		})
	}
}

// Without an owner the executor keeps its previous behaviour.
func TestUnownedExecutorKeepsGlobalPrefixBehaviour(t *testing.T) {
	tr, f, _ := fixture(t)
	f.orders = []*binance.Order{stopOrder(-1)}
	if p, err := tr.GetProtection("AAPLBUSDT"); err != nil || p == nil {
		t.Fatalf("%+v %v", p, err)
	}
}
