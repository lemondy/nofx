package breakout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Vary the within-day noise so the IC series has finite, nonzero dispersion.
// Day shocks dominate raw returns but do not change within-day ranks.
func reviewRankICSamples(start time.Time, blocks, symbols int) []shortSample {
	start = start.UTC().Truncate(48 * time.Hour)
	var samples []shortSample
	for block := 0; block < blocks; block++ {
		for symbol := 0; symbol < symbols; symbol++ {
			y := float64(symbol)
			if block%3 != 0 && symbol < 2 {
				y = float64(1 - symbol)
			}
			samples = append(samples, shortSample{
				TS:     start.Add(time.Duration(block) * 48 * time.Hour).UnixMilli(),
				Symbol: fmt.Sprintf("COIN%02dUSDT", symbol), Price: 100,
				Evaluated: true, LabelVersion: 2,
				Outcome: 100*float64(block) + y,
				Components: map[string]float64{
					"structure": float64(symbol), "overbought": -float64(symbol), "stretch": 50,
				},
			})
		}
	}
	return samples
}

func TestRankICMarketTimingTrap(t *testing.T) {
	samples := reviewRankICSamples(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 40, 10)
	var dayX, dayY []float64
	for i := range samples {
		block, symbol := i/10, i%10
		shock := float64(block * 100)
		samples[i].Outcome = shock // all symbols share the market move
		samples[i].Components = map[string]float64{"structure": shock + float64(symbol)}
		if symbol == 0 {
			dayX, dayY = append(dayX, shock+4.5), append(dayY, shock)
		}
	}
	if corr := pearson(dayX, dayY); corr < 0.999 {
		t.Fatalf("fixture must fool day-mean correlation: %v", corr)
	}
	_, stats, ok := updateShortWeightsWithDiagnostics(samples, DefaultShortWeights(), shortTunerEta)
	if ok || stats["structure"].N != 0 {
		t.Fatalf("market timing admitted as cross-sectional signal: %+v", stats["structure"])
	}

	// Also retain blocks with variable returns but balanced unrelated ranks:
	// opposite ICs cancel while day means still track the market shock.
	for i := range samples {
		block, symbol := i/10, i%10
		y := symbol
		if block%2 != 0 {
			y = 9 - symbol
		}
		samples[i].Outcome += float64(y)
	}
	_, stats, ok = updateShortWeightsWithDiagnostics(samples, DefaultShortWeights(), shortTunerEta)
	if ok || stats["structure"].N != 40 || math.Abs(stats["structure"].MeanIC) > 1e-12 {
		t.Fatalf("balanced cross-sectional noise admitted: %+v", stats["structure"])
	}
}

func TestRankICGenuineCrossSectionalSignal(t *testing.T) {
	samples := reviewRankICSamples(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 40, 10)
	base := DefaultShortWeights()
	weights, stats, ok := updateShortWeightsWithDiagnostics(samples, base, shortTunerEta)
	stat := stats["structure"]
	if !ok || stat.N != 40 || stat.MeanIC < 0.9 || stat.T == nil || *stat.T <= 3 || weights["structure"] <= base["structure"] {
		t.Fatalf("predictive ranks did not raise weight: weights=%v stat=%+v", weights, stat)
	}
	if weights["overbought"] >= base["overbought"] || !stats["overbought"].significant() {
		t.Fatal("negative rank IC must reduce weight")
	}
	if _, _, ok := updateShortWeightsWithDiagnostics(samples[:29*10], base, shortTunerEta); ok {
		t.Fatal("29 blocks bypassed minimum")
	}
	// Odd UTC days must not add evidence, even if they are strongly predictive.
	for _, sample := range append([]shortSample(nil), samples...) {
		sample.TS += (24 * time.Hour).Milliseconds()
		samples = append(samples, sample)
	}
	_, stats, _ = updateShortWeightsWithDiagnostics(samples, base, shortTunerEta)
	if stats["structure"].N != 40 {
		t.Fatalf("adjacent overlapping days counted: %+v", stats["structure"])
	}
}

func TestRankICRepeatedSymbolCountsOnce(t *testing.T) {
	for _, distinct := range []int{7, 8} {
		t.Run(fmt.Sprintf("%d_distinct", distinct), func(t *testing.T) {
			samples := reviewRankICSamples(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 40, distinct)
			var duplicates []shortSample
			for _, sample := range samples {
				if sample.Symbol == "COIN00USDT" {
					for hour := 1; hour < 24; hour++ {
						copy := sample
						copy.TS += int64(hour) * time.Hour.Milliseconds()
						duplicates = append(duplicates, copy)
					}
				}
			}
			_, before, _ := updateShortWeightsWithDiagnostics(samples, DefaultShortWeights(), shortTunerEta)
			_, after, ok := updateShortWeightsWithDiagnostics(append(samples, duplicates...), DefaultShortWeights(), shortTunerEta)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("identical hourly repeats changed symbol evidence: before=%+v after=%+v", before, after)
			}
			if distinct == 7 && (ok || after["structure"].N != 0) {
				t.Fatal("24 samples of one symbol plus six others bypassed eight-symbol minimum")
			}
			if distinct == 8 && (!ok || after["structure"].N != 40) {
				t.Fatal("one repeated symbol plus seven others should qualify as eight symbols")
			}
		})
	}
}

func TestRankICRepeatedSymbolUsesMeans(t *testing.T) {
	samples := reviewRankICSamples(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 30, 8)
	_, want, _ := updateShortWeightsWithDiagnostics(samples, DefaultShortWeights(), shortTunerEta)
	var repeats []shortSample
	for i := range samples {
		if samples[i].Symbol != "COIN00USDT" {
			continue
		}
		repeat := samples[i]
		repeat.TS += time.Hour.Milliseconds()
		repeat.Components = map[string]float64{"structure": 100}
		repeat.Outcome += 100
		samples[i].Components = map[string]float64{"structure": -100}
		samples[i].Outcome -= 100
		repeats = append(repeats, repeat)
	}
	_, got, _ := updateShortWeightsWithDiagnostics(append(samples, repeats...), DefaultShortWeights(), shortTunerEta)
	if !reflect.DeepEqual(want["structure"], got["structure"]) {
		t.Fatalf("symbol means not used: want=%+v got=%+v", want["structure"], got["structure"])
	}
}

func TestRankICTiesAndZeroVariance(t *testing.T) {
	if got := averageRanks([]float64{4, 1, 1, 2, 4}); !reflect.DeepEqual(got, []float64{4.5, 1.5, 1.5, 3, 4.5}) {
		t.Fatalf("ties did not get average ranks: %v", got)
	}
	if got, ok := spearman([]float64{1, 1, 2, 3}, []float64{1, 2, 3, 4}); !ok || math.Abs(got-3/math.Sqrt(10)) > 1e-12 {
		t.Fatalf("wrong tied Spearman: %v %v", got, ok)
	}
	for _, pair := range [][2][]float64{{{1, 1, 1}, {1, 2, 3}}, {{1, 2, 3}, {2, 2, 2}}} {
		if _, ok := spearman(pair[0], pair[1]); ok {
			t.Fatal("constant ranks must have undefined IC")
		}
	}
	samples := reviewRankICSamples(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 40, 10)
	_, stats, _ := updateShortWeightsWithDiagnostics(samples, DefaultShortWeights(), shortTunerEta)
	if stats["stretch"].N != 0 {
		t.Fatal("constant-tied component counted toward retained blocks")
	}
	// Perfect IC series have zero standard error and still produce valid JSON.
	stat := summarizeRankIC([]float64{1, 1, 1})
	if stat.T != nil || !stat.TInfinite {
		t.Fatalf("constant nonzero IC should have limiting infinite t: %+v", stat)
	}
	if _, err := json.Marshal(stat); err != nil {
		t.Fatal(err)
	}
	zero := summarizeRankIC([]float64{0, 0, 0})
	if zero.T == nil || *zero.T != 0 || zero.TInfinite {
		t.Fatalf("constant zero IC is not evidence: %+v", zero)
	}
}

func TestRankICSeriesTStatistic(t *testing.T) {
	stat := summarizeRankIC([]float64{0.1, 0.2, 0.3})
	if stat.N != 3 || math.Abs(stat.MeanIC-0.2) > 1e-12 || stat.T == nil || math.Abs(*stat.T-2*math.Sqrt(3)) > 1e-12 {
		t.Fatalf("incorrect sample-sd t statistic: %+v", stat)
	}
	for _, tc := range []struct {
		n           int
		t           float64
		significant bool
	}{{29, 100, false}, {30, 3, false}, {30, 3.01, true}, {30, -3.01, true}} {
		stat := ICStat{N: tc.n, T: &tc.t}
		if stat.significant() != tc.significant {
			t.Fatalf("gate failed for n=%d t=%v", tc.n, tc.t)
		}
	}
}

func TestRankICSamplingAndLegacyRank(t *testing.T) {
	oldPath := shortTuningPath
	shortTuningPath = filepath.Join(t.TempDir(), "signals.jsonl")
	t.Cleanup(func() { shortTuningPath = oldPath })
	// Literal legacy JSON omits rank, exactly as old journal writers did.
	legacy := `{"ts":1000,"symbol":"LEGACYUSDT","score":60,"price":100,"components":{"structure":50},"evaluated":true,"outcome":2}` + "\n"
	if err := os.WriteFile(shortTuningPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	var signals []ShortSignal
	for i := 0; i < 50; i++ {
		signals = append(signals, ShortSignal{Symbol: fmt.Sprintf("COIN%02dUSDT", i), Score: float64(100 - i), Price: 100})
	}
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	SampleShortSignals(signals, now)
	samples := readSamples()
	if len(samples) != 31 || samples[0].Rank != 0 || samples[0].Outcome != 2 {
		t.Fatalf("legacy/sample count mismatch: %d %+v", len(samples), samples[0])
	}
	for i, sample := range samples[1:] {
		if sample.Rank != i+1 || sample.Symbol != signals[i].Symbol {
			t.Fatalf("snapshot rank/order mismatch: %+v", sample)
		}
	}
	var raw map[string]interface{}
	data, err := os.ReadFile(shortTuningPath)
	if err != nil {
		t.Fatal(err)
	}
	// Read the first new JSON line to verify the serialized field name.
	lines := bytes.Split(data, []byte{'\n'})
	if err := json.Unmarshal(lines[1], &raw); err != nil || raw["rank"] != float64(1) {
		t.Fatalf("rank not serialized: %s (%v)", lines[1], err)
	}
	SampleShortSignals(signals, now.Add(30*time.Minute))
	if len(readSamples()) != 31 {
		t.Fatal("hourly sampling throttle changed")
	}
	SampleShortSignals(signals[:5], now.Add(time.Hour))
	if len(readSamples()) != 36 {
		t.Fatal("short snapshots should journal all available signals")
	}
}
