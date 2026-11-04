package stockengine

import (
	"os"
	"path/filepath"
	"testing"
)

// Captured from the long-only prompts before adding paper shorts.
func TestShortOffPromptGolden(t *testing.T) {
	c := testContext()
	holding(c)
	c.Snapshots[0].Cycle = &CycleAssessment{Phase: PhaseWedgePop, LongSetup: true, Score: 72, RS63Pct: 12}
	c.Universe = []ScanResult{{Symbol: "AAPLBUSDT", Cycle: *c.Snapshots[0].Cycle, Momentum: MomentumAssessment{Valid: true, AboveSMA200: true, Mom12_1Pct: 20}}}
	for _, mode := range []string{"", "off"} {
		c.Config.ShortMode = mode
		for _, lang := range []string{"en", "zh"} {
			for kind, got := range map[string]string{"system": BuildSystemPrompt(c.Config, ResolvePreset(c.Config), lang), "user": BuildUserPrompt(c, lang)} {
				path := filepath.Join("testdata", "short_off_"+lang+"_"+kind+".golden")
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if got != string(want) {
					t.Fatalf("%s %s prompt differs from original golden", lang, kind)
				}
			}
		}
	}
}
