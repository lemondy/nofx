package hyperliquid

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type assetMappingSnapshot struct {
	assets    map[string]AssetMapping
	fetchedAt time.Time
}

var assetMappingCache = struct {
	sync.RWMutex
	byURL map[string]assetMappingSnapshot
}{byURL: make(map[string]assetMappingSnapshot)}

func rememberAssetMappings(apiURL string, universe []AssetInfo) {
	assetMappingCache.Lock()
	assetMappingCache.byURL[apiURL] = assetMappingSnapshot{assets: AssetMappings(universe), fetchedAt: time.Now()}
	assetMappingCache.Unlock()
}

func (c *Client) resolveCandleAsset(ctx context.Context, symbol string) (AssetMapping, error) {
	assetMappingCache.RLock()
	snapshot := assetMappingCache.byURL[c.apiURL]
	assetMappingCache.RUnlock()
	known, knownErr := ResolveAsset(symbol, snapshot.assets)
	if knownErr == nil && time.Since(snapshot.fetchedAt) < cacheDuration {
		return known, nil
	}
	meta, err := c.GetMeta(ctx)
	if err != nil {
		// The name/unit of a known contract remains usable for position data
		// during a metadata outage. Never guess a name for an unknown unit.
		if knownErr == nil {
			return known, nil
		}
		return AssetMapping{}, fmt.Errorf("resolve Hyperliquid contract: %w", err)
	}
	return ResolveAsset(symbol, AssetMappings(meta.Universe))
}

// AssetMapping identifies one venue contract. Multiplier describes the venue
// unit; callers must keep its price and size in that unit, never alias kPEPE,
// 1000PEPE and PEPE or silently rescale them.
type AssetMapping struct {
	Symbol     string
	Name       string
	AssetID    int
	Multiplier int
	IsDelisted bool
}

func InternalSymbol(name string) string {
	base := strings.ToUpper(strings.TrimSpace(name))
	base = strings.ReplaceAll(base, "_", "")
	base = strings.ReplaceAll(base, "-SWAP", "")
	base = strings.ReplaceAll(base, "-", "")
	if !strings.HasSuffix(base, "USDT") {
		base += "USDT"
	}
	return base
}

// AssetMappings uses the original universe index, including delisted entries:
// removing one from a candidate list must not shift any order's asset ID or
// prevent an existing position from being resolved for protection/exit.
func AssetMappings(universe []AssetInfo) map[string]AssetMapping {
	out := make(map[string]AssetMapping, len(universe))
	ambiguous := make(map[string]bool)
	for i, asset := range universe {
		if strings.TrimSpace(asset.Name) == "" {
			continue
		}
		symbol := InternalSymbol(asset.Name)
		if _, exists := out[symbol]; exists || ambiguous[symbol] {
			delete(out, symbol)
			ambiguous[symbol] = true
			continue
		}
		multiplier := 1
		if len(asset.Name) > 1 && asset.Name[0] == 'k' && asset.Name[1] >= 'A' && asset.Name[1] <= 'Z' {
			multiplier = 1000
		} else if strings.HasPrefix(asset.Name, "1000") && len(asset.Name) > 4 && asset.Name[4] >= 'A' && asset.Name[4] <= 'Z' {
			multiplier = 1000
		}
		out[symbol] = AssetMapping{Symbol: symbol, Name: asset.Name, AssetID: i, Multiplier: multiplier, IsDelisted: asset.IsDelisted}
	}
	return out
}

func ResolveAsset(symbol string, mappings map[string]AssetMapping) (AssetMapping, error) {
	if strings.TrimSpace(symbol) != "" {
		if asset, ok := mappings[InternalSymbol(symbol)]; ok {
			return asset, nil
		}
	}
	return AssetMapping{}, fmt.Errorf("unknown Hyperliquid contract %q", symbol)
}
