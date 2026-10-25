package usstock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"nofx/logger"
)

const symbolsDiskVersion = 1

// SymbolsCachePath can be configured before starting registry requests. Empty
// disables disk caching. Both source URLs must match before a file can be used.
var SymbolsCachePath = "data/cache/usstock_symbols.json"

type symbolsDisk struct {
	Version        int          `json:"version"`
	SpotBaseURL    string       `json:"spot_base_url"`
	FuturesBaseURL string       `json:"futures_base_url"`
	FetchedAt      time.Time    `json:"fetched_at"`
	Symbols        []SymbolInfo `json:"symbols"`
}

// Caller holds symbolCache's mutex. Expired disk data is retained for the same
// failed-refresh fallback as an expired in-memory registry.
func readSymbolsDisk() {
	if SymbolsCachePath == "" {
		return
	}
	data, err := os.ReadFile(SymbolsCachePath)
	if err != nil {
		return
	}
	var disk symbolsDisk
	if json.Unmarshal(data, &disk) != nil || disk.Version != symbolsDiskVersion || disk.SpotBaseURL != spotBaseURL || disk.FuturesBaseURL != futuresBaseURL || disk.FetchedAt.IsZero() || disk.FetchedAt.After(nowFunc()) || len(disk.Symbols) == 0 {
		return
	}
	symbolCache.symbols, symbolCache.loaded = disk.Symbols, disk.FetchedAt
}

func writeSymbolsDisk() {
	if SymbolsCachePath == "" {
		return
	}
	disk := symbolsDisk{symbolsDiskVersion, spotBaseURL, futuresBaseURL, symbolCache.loaded, symbolCache.symbols}
	if err := writeSymbolsFile(SymbolsCachePath, disk); err != nil {
		logger.Warnf("US stock registry cache write failed (%s): %v", SymbolsCachePath, err)
	}
}

func writeSymbolsFile(path string, disk symbolsDisk) error {
	data, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".usstock-symbols-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
