package notify

import (
	"strings"
	"testing"
)

// formatMessage must produce the HTML envelope (bold badge+trader, dim stamp)
// and callers' dynamic values must survive Telegram's HTML parser.
func TestFormatMessageHTMLEnvelope(t *testing.T) {
	out := formatMessage("ORDER", "mac-nofx", "<b>🔻 开空 BRUSDT</b>\n数量 <code>164</code>")
	if !strings.HasPrefix(out, "<b>📊 mac-nofx</b> · <i>") {
		t.Fatalf("missing badge/title/stamp envelope: %q", out)
	}
	if !strings.Contains(out, "<b>🔻 开空 BRUSDT</b>") {
		t.Fatalf("body must be passed through as HTML: %q", out)
	}
}

func TestEscape(t *testing.T) {
	got := Escape(`<err>&"x"`)
	want := "&lt;err&gt;&amp;\"x\""
	if got != want {
		t.Fatalf("Escape = %q, want %q", got, want)
	}
	// Plain symbols pass through untouched.
	if s := Escape("xyz:HOOD"); s != "xyz:HOOD" {
		t.Fatalf("plain symbol changed: %q", s)
	}
}

func TestKindBadge(t *testing.T) {
	cases := map[string]string{"ORDER": "📊", "RISK": "🛡️", "ALERT": "🚨", "WHATEVER": "📣"}
	for kind, want := range cases {
		if got := kindBadge(kind); got != want {
			t.Fatalf("kindBadge(%q) = %q, want %q", kind, got, want)
		}
	}
}
