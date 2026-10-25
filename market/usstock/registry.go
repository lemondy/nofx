package usstock

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var symbolCache struct {
	sync.Mutex
	symbols []SymbolInfo
	loaded  time.Time
}

type exchangeInfo struct {
	Symbols []struct {
		Symbol         string `json:"symbol"`
		BaseAsset      string `json:"baseAsset"`
		QuoteAsset     string `json:"quoteAsset"`
		Status         string `json:"status"`
		UnderlyingType string `json:"underlyingType"`
		Filters        []struct {
			Type        string `json:"filterType"`
			TickSize    string `json:"tickSize"`
			StepSize    string `json:"stepSize"`
			MinQty      string `json:"minQty"`
			MinNotional string `json:"minNotional"`
		} `json:"filters"`
	} `json:"symbols"`
}

// ListSymbols returns a copy of the US bStock registry, refreshed every 24h.
// A failed refresh preserves the last good set; a failed initial load errors.
func ListSymbols(ctx context.Context) ([]SymbolInfo, error) {
	symbolCache.Lock()
	defer symbolCache.Unlock()
	if symbolCache.symbols != nil && nowFunc().Sub(symbolCache.loaded) < 24*time.Hour {
		return append([]SymbolInfo(nil), symbolCache.symbols...), nil
	}
	symbols, err := loadSymbols(ctx)
	if err != nil {
		if symbolCache.symbols != nil {
			return append([]SymbolInfo(nil), symbolCache.symbols...), nil
		}
		return nil, err
	}
	symbolCache.symbols, symbolCache.loaded = symbols, nowFunc()
	return append([]SymbolInfo(nil), symbols...), nil
}

// LookupSymbol looks up an exact Binance spot symbol. False also means the
// registry could not be loaded: callers must refuse to open in that case.
func LookupSymbol(ctx context.Context, symbol string) (SymbolInfo, bool) {
	symbols, err := ListSymbols(ctx)
	if err != nil {
		return SymbolInfo{}, false
	}
	for _, info := range symbols {
		if info.Symbol == symbol {
			return info, true
		}
	}
	return SymbolInfo{}, false
}

func loadSymbols(ctx context.Context) ([]SymbolInfo, error) {
	var spot, futures exchangeInfo
	if err := fetchJSON(ctx, spotBaseURL, "/api/v3/exchangeInfo", nil, false, &spot); err != nil {
		return nil, fmt.Errorf("spot registry: %w", err)
	}
	if err := fetchJSON(ctx, futuresBaseURL, "/fapi/v1/exchangeInfo", nil, false, &futures); err != nil {
		return nil, fmt.Errorf("futures registry: %w", err)
	}
	equities := make(map[string]bool)
	for _, s := range futures.Symbols {
		if s.UnderlyingType == "EQUITY" {
			equities[s.BaseAsset] = true
		}
	}
	symbols := make([]SymbolInfo, 0)
	for _, s := range spot.Symbols {
		ticker := strings.TrimSuffix(s.BaseAsset, "B")
		if s.Status != "TRADING" || s.QuoteAsset != "USDT" || !strings.HasSuffix(s.BaseAsset, "B") || !equities[ticker] {
			continue
		}
		// Yahoo denotes share classes with a hyphen (e.g. BRK.B -> BRK-B).
		info := SymbolInfo{Symbol: s.Symbol, BaseAsset: s.BaseAsset, Underlying: strings.ReplaceAll(ticker, ".", "-"), Status: s.Status}
		hasNotional := false
		for _, f := range s.Filters {
			var fields []*float64
			var values []string
			switch f.Type {
			case "PRICE_FILTER":
				fields, values = []*float64{&info.TickSize}, []string{f.TickSize}
			case "LOT_SIZE":
				fields, values = []*float64{&info.StepSize, &info.MinQty}, []string{f.StepSize, f.MinQty}
			case "NOTIONAL":
				fields, values = []*float64{&info.MinNotional}, []string{f.MinNotional}
				hasNotional = true
			}
			for i, value := range values {
				n, err := strconv.ParseFloat(value, 64)
				if err != nil {
					return nil, fmt.Errorf("%s %s: %w", s.Symbol, f.Type, err)
				}
				*fields[i] = n
			}
		}
		if !hasNotional {
			for _, f := range s.Filters {
				if f.Type == "MIN_NOTIONAL" {
					n, err := strconv.ParseFloat(f.MinNotional, 64)
					if err != nil {
						return nil, fmt.Errorf("%s MIN_NOTIONAL: %w", s.Symbol, err)
					}
					info.MinNotional = n
				}
			}
		}
		symbols = append(symbols, info)
	}
	if len(symbols) == 0 {
		return nil, fmt.Errorf("exchangeInfo contains no US bStock symbols")
	}
	sort.Slice(symbols, func(i, j int) bool { return symbols[i].Symbol < symbols[j].Symbol })
	return symbols, nil
}
