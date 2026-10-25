package api

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"nofx/logger"
)

const factorMiningMaxFileSize = 8 << 20 // 8 MiB per file, including runs.jsonl.

const (
	factorMiningMaxZooFileSize = 128 << 20
	factorMiningZooGroupLimit  = 150
)

type factorMiningZooCacheEntry struct {
	mtime time.Time
	size  int64
	data  json.RawMessage
}

var factorMiningZooCache = struct {
	sync.Mutex
	entries map[string]factorMiningZooCacheEntry
}{entries: make(map[string]factorMiningZooCacheEntry)}

func factorMiningDir() string {
	if dir := os.Getenv("FACTOR_MINING_DIR"); dir != "" {
		return dir
	}
	return "data/factor_mining"
}

// readFactorMiningFile only receives server-defined paths, never request input.
// Limit the read as well as checking stat, since the writer may append meanwhile.
func readFactorMiningFile(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf("[factor-mining] Cannot open %s: %v", path, err)
		}
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		logger.Warnf("[factor-mining] Cannot stat %s: %v", path, err)
		return nil
	}
	if info.Size() > factorMiningMaxFileSize {
		logger.Warnf("[factor-mining] Ignoring %s: exceeds 8 MiB", path)
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(f, factorMiningMaxFileSize+1))
	if err != nil {
		logger.Warnf("[factor-mining] Cannot read %s: %v", path, err)
		return nil
	}
	if len(data) > factorMiningMaxFileSize {
		logger.Warnf("[factor-mining] Ignoring %s: exceeds 8 MiB", path)
		return nil
	}
	return data
}

func readFactorMiningJSON(path string) json.RawMessage {
	data := readFactorMiningFile(path)
	if data == nil {
		return nil
	}
	if !json.Valid(data) {
		logger.Warnf("[factor-mining] Invalid JSON in %s", path)
		return nil
	}
	return json.RawMessage(data)
}

// readFactorMiningZoo caches the trimmed response, never writing to the source.
// Hold the lock through parsing so concurrent requests only parse a new file once.
func readFactorMiningZoo(path string) json.RawMessage {
	path, err := filepath.Abs(path)
	if err != nil {
		logger.Warnf("[factor-mining] Cannot resolve zoo path: %v", err)
		return nil
	}
	factorMiningZooCache.Lock()
	defer factorMiningZooCache.Unlock()

	f, err := os.Open(path)
	if err != nil {
		delete(factorMiningZooCache.entries, path)
		if !os.IsNotExist(err) {
			logger.Warnf("[factor-mining] Cannot open %s: %v", path, err)
		}
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		delete(factorMiningZooCache.entries, path)
		logger.Warnf("[factor-mining] Cannot stat %s: %v", path, err)
		return nil
	}
	if cached, ok := factorMiningZooCache.entries[path]; ok && cached.mtime.Equal(info.ModTime()) && cached.size == info.Size() {
		return cached.data
	}
	delete(factorMiningZooCache.entries, path)
	if info.Size() > factorMiningMaxZooFileSize {
		logger.Warnf("[factor-mining] Ignoring %s: exceeds 128 MiB", path)
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(f, factorMiningMaxZooFileSize+1))
	if err != nil {
		logger.Warnf("[factor-mining] Cannot read %s: %v", path, err)
		return nil
	}
	if len(data) > factorMiningMaxZooFileSize {
		logger.Warnf("[factor-mining] Ignoring %s: exceeds 128 MiB", path)
		return nil
	}
	trimmed, err := trimFactorMiningZoo(data)
	if err != nil {
		logger.Warnf("[factor-mining] Invalid JSON in %s: %v", path, err)
		return nil
	}
	factorMiningZooCache.entries[path] = factorMiningZooCacheEntry{info.ModTime(), info.Size(), trimmed}
	return trimmed
}

func trimFactorMiningZoo(data []byte) (json.RawMessage, error) {
	// RawMessage preserves unknown fields and numeric precision in the response.
	var zoo map[string]json.RawMessage
	if err := json.Unmarshal(data, &zoo); err != nil {
		return nil, err
	}
	if zoo == nil {
		return nil, nil
	}
	var factors []json.RawMessage
	if raw := zoo["factors"]; raw != nil {
		if err := json.Unmarshal(raw, &factors); err != nil {
			return nil, err
		}
	}
	type metrics struct {
		T *float64 `json:"t"`
	}
	type group struct {
		horizon int
		status  string
	}
	type rankedFactor struct {
		index int
		id    string
		t     float64
	}
	groups := make(map[group][]rankedFactor)
	full := make([]bool, len(factors))
	keep := make([]bool, len(factors))
	for i, raw := range factors {
		var factor struct {
			ID      string                     `json:"id"`
			Horizon int                        `json:"horizon"`
			Status  string                     `json:"status"`
			Train   *metrics                   `json:"train"`
			Val     *metrics                   `json:"val"`
			Forward map[string]json.RawMessage `json:"forward"`
		}
		if err := json.Unmarshal(raw, &factor); err != nil {
			return nil, err
		}
		if factor.Status == "accepted" || len(factor.Forward) > 0 {
			full[i], keep[i] = true, true
			continue
		}
		t := math.Inf(-1)
		if factor.Val != nil && factor.Val.T != nil {
			t = *factor.Val.T
		} else if factor.Train != nil && factor.Train.T != nil {
			t = *factor.Train.T
		}
		key := group{factor.Horizon, factor.Status}
		groups[key] = append(groups[key], rankedFactor{i, factor.ID, t})
	}
	for _, entries := range groups {
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].t != entries[j].t {
				return entries[i].t > entries[j].t
			}
			return entries[i].id < entries[j].id
		})
		for _, entry := range entries[:min(len(entries), factorMiningZooGroupLimit)] {
			keep[entry.index] = true
		}
	}
	// Emit in original order, independent of map iteration order above.
	returned := make([]json.RawMessage, 0)
	for i, raw := range factors {
		if !keep[i] {
			continue
		}
		if !full[i] {
			var factor map[string]json.RawMessage
			if err := json.Unmarshal(raw, &factor); err != nil {
				return nil, err
			}
			delete(factor, "ic_monthly")
			var err error
			raw, err = json.Marshal(factor)
			if err != nil {
				return nil, err
			}
		}
		returned = append(returned, raw)
	}
	var err error
	zoo["factors"], err = json.Marshal(returned)
	if err != nil {
		return nil, err
	}
	zoo["factors_total"] = json.RawMessage(strconv.Itoa(len(factors)))
	zoo["factors_returned"] = json.RawMessage(strconv.Itoa(len(returned)))
	return json.Marshal(zoo)
}

func (s *Server) handleFactorMiningOverview(c *gin.Context) {
	dir := factorMiningDir()
	c.JSON(http.StatusOK, gin.H{
		"status": readFactorMiningJSON(filepath.Join(dir, "status.json")),
		"zoo": gin.H{
			"crypto": readFactorMiningZoo(filepath.Join(dir, "crypto", "zoo.json")),
			"us":     readFactorMiningZoo(filepath.Join(dir, "us", "zoo.json")),
		},
		"ensemble": gin.H{
			"crypto": readFactorMiningJSON(filepath.Join(dir, "crypto", "ensemble.json")),
			"us":     readFactorMiningJSON(filepath.Join(dir, "us", "ensemble.json")),
		},
	})
}

func (s *Server) handleFactorMiningRuns(c *gin.Context) {
	limit, err := strconv.Atoi(c.Query("limit"))
	if err != nil || limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	runs := make([]json.RawMessage, 0, limit)
	path := filepath.Join(factorMiningDir(), "runs.jsonl")
	if data := readFactorMiningFile(path); len(data) > 0 {
		// A terminal newline ends the last record; it is not an extra line.
		data = bytes.TrimSuffix(data, []byte("\n"))
		// The log is append-only: reverse the last limit physical lines to
		// return newest first. Bad lines do not pull older records into view.
		skipped := 0
		for seen, end := 0, len(data); seen < limit && end > 0; seen++ {
			start := bytes.LastIndexByte(data[:end], '\n') + 1
			line := bytes.TrimSpace(data[start:end])
			end = start - 1
			if !json.Valid(line) {
				skipped++
				continue
			}
			runs = append(runs, json.RawMessage(line))
		}
		if skipped > 0 {
			logger.Warnf("[factor-mining] Skipped %d invalid JSON lines in %s", skipped, path)
		}
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}
