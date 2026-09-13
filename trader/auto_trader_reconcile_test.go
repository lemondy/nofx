package trader

import (
	"strings"
	"testing"
)

// Entry-tag ownership: the exchange reports the FULL client order ID —
// broker prefix + "lim-<hash4>-<ms>" — and it must bind an order to exactly
// one trader; untagged (grid/protective/legacy) orders are never owned —
// reconciliation must leave those alone.
func TestEntryOrderOwnedBy(t *testing.T) {
	const traderA = "d7ff3764-f6b845ba_deepseek_1787982996"
	const traderB = "other-trader-id"

	idA := traderHash4(traderA)
	own := "x-KzrpZaP9lim-" + idA + "-1788966018463"
	other := "x-KzrpZaP9lim-" + traderHash4(traderB) + "-1788966018463"

	cases := []struct {
		name     string
		clientID string
		traderID string
		want     bool
	}{
		{"own tag (full exchange form)", own, traderA, true},
		{"other trader's tag", other, traderA, false},
		{"own tag under other trader", own, traderB, false},
		{"grid order", "x-KzrpZaP9grid-3-123456", traderA, false},
		{"broker prefix only", "x-KzrpZaP91234567890123abcdef12", traderA, false},
		{"empty client id", "", traderA, false},
		{"lim prefix but no hash", "x-KzrpZaP9lim-1788966018463", traderA, false},
		{"no broker prefix, not a tag", "x-OTHERBR123456", traderA, false},
	}
	for _, tc := range cases {
		if got := entryOrderOwnedBy(tc.clientID, tc.traderID); got != tc.want {
			t.Errorf("%s: got %v, want %v (client %q)", tc.name, got, tc.want, tc.clientID)
		}
	}
}

// Tag generation: fixed 22-char budget so the broker prefix + tag fits the
// 32-char cap; the lim-<hash> head survives right-truncation of the
// millisecond timestamp (deterministic here — entryClientID embeds time, so
// assert the shape, not an exact string).
func TestEntryClientIDShape(t *testing.T) {
	at := &AutoTrader{id: "d7ff3764-f6b845ba_deepseek_1787982996"}
	id := at.entryClientID()
	if len(id) != 22 {
		t.Errorf("entry client ID must be exactly 22 chars (11 broker prefix + 11 tag head), got %d: %q", len(id), id)
	}
	if !strings.HasPrefix(id, "lim-"+traderHash4(at.id)+"-") {
		t.Errorf("entry client ID %q must start with lim-<trader hash4>-", id)
	}
}
