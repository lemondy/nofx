package nofxos

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// PriceRankingItem represents single coin price ranking data
type PriceRankingItem struct {
	Pair         string  `json:"pair"`
	Symbol       string  `json:"symbol"`
	PriceDelta   float64 `json:"price_delta"` // Decimal format: 0.0723 = 7.23%
	Price        float64 `json:"price"`
	FutureFlow   float64 `json:"future_flow"`
	SpotFlow     float64 `json:"spot_flow"`
	OI           float64 `json:"oi"`
	OIDelta      float64 `json:"oi_delta"`
	OIDeltaValue float64 `json:"oi_delta_value"`
}

// PriceRankingDuration contains top gainers and losers for a single duration
type PriceRankingDuration struct {
	Top []PriceRankingItem `json:"top"`
	Low []PriceRankingItem `json:"low"`
}

// PriceRankingResponse is the API response structure
type PriceRankingResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Durations []string                        `json:"durations"`
		Limit     int                             `json:"limit"`
		Data      map[string]PriceRankingDuration `json:"data"`
	} `json:"data"`
}

// PriceRankingData contains price ranking data for multiple durations
type PriceRankingData struct {
	Durations map[string]*PriceRankingDuration `json:"durations"`
	FetchedAt time.Time                        `json:"fetched_at"`
}

// GetPriceRanking retrieves price ranking data (gainers/losers)
func (c *Client) GetPriceRanking(durations string, limit int) (*PriceRankingData, error) {
	if durations == "" {
		durations = "1h"
	}
	if limit <= 0 {
		limit = 10
	}

	endpoint := fmt.Sprintf("/api/price/ranking?duration=%s&limit=%d", durations, limit)

	body, err := c.doRequest(endpoint)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	var response PriceRankingResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("JSON parsing failed: %w", err)
	}

	if !response.Success {
		return nil, fmt.Errorf("API returned failure status")
	}

	result := &PriceRankingData{
		Durations: make(map[string]*PriceRankingDuration),
		FetchedAt: time.Now(),
	}

	for duration, data := range response.Data.Data {
		d := data // Create a copy to avoid pointer issues
		result.Durations[duration] = &d
	}

	log.Printf("✓ Fetched Price ranking data for %d durations", len(result.Durations))

	return result, nil
}

// FormatPriceRankingForAI formats price ranking data for AI consumption.
// interesting marks symbols warranting full rows (candidates/open positions);
// fresh60m holds THIS cycle's rolling-60m change for those symbols (the
// ranking table is a snapshot that can be minutes stale on violent movers —
// full-row symbols get the fresh, signal-aligned figure instead).
func FormatPriceRankingForAI(data *PriceRankingData, lang Language, interesting map[string]bool, fresh60m map[string]float64) string {
	if data == nil || len(data.Durations) == 0 {
		return ""
	}

	if lang == LangChinese {
		return formatPriceRankingZH(data, interesting, fresh60m)
	}
	return formatPriceRankingEN(data, interesting, fresh60m)
}

func priceInteresting(item PriceRankingItem, interesting map[string]bool) bool {
	return interesting[item.Symbol] || interesting[item.Pair]
}

// writePriceSide renders one gainers/losers side: full rows for interesting
// symbols (fresh60m overrides the stale snapshot for 1h rows), others
// collapsed to a top-3 headline.
func writePriceSide(sb *strings.Builder, items []PriceRankingItem, hasFlow, hasOI bool, interesting map[string]bool, fresh60m map[string]float64, duration, label string) {
	// Two passes: full table first, headline after — a single pass lets the
	// "其余前列:" text land mid-table and breaks the markdown.
	var fullRows, rest []PriceRankingItem
	for _, item := range items {
		if priceInteresting(item, interesting) {
			fullRows = append(fullRows, item)
		} else {
			rest = append(rest, item)
		}
	}
	if len(fullRows) > 0 {
		sb.WriteString(rankingHeaderZH(hasFlow, hasOI))
		for _, item := range fullRows {
			sb.WriteString(rankingRowZH(item, hasFlow, hasOI, fresh60m, duration))
		}
	}
	if len(rest) > 0 {
		sb.WriteString(label + ": ")
		for i, item := range rest {
			if i >= 3 {
				break
			}
			if i > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString(fmt.Sprintf("%s(%+.1f%%)", item.Symbol, item.PriceDelta*100))
		}
		sb.WriteString("\n")
	}
}

// reclassifyFreshFlips returns the Top/Low boards with items whose fresh 1h
// value has crossed zero moved to the opposite board. The fresh-60m override
// updates the displayed number, but board membership was decided by the
// possibly-stale snapshot — without this, a negative number can sit in the
// gainers board and vice versa. Only 1h rows carry fresh values; other
// durations pass through untouched. Operates on copies, never the input.
func reclassifyFreshFlips(top, low []PriceRankingItem, fresh60m map[string]float64, duration string) ([]PriceRankingItem, []PriceRankingItem) {
	if duration != "1h" || len(fresh60m) == 0 {
		return top, low
	}
	freshDelta := func(it PriceRankingItem) (float64, bool) {
		v, ok := fresh60m[it.Symbol]
		return v, ok
	}
	inBoard := func(items []PriceRankingItem, symbol string) bool {
		for _, it := range items {
			if it.Symbol == symbol {
				return true
			}
		}
		return false
	}
	var keptTop, keptLow, flipped []PriceRankingItem
	for _, it := range top {
		if v, ok := freshDelta(it); ok && v < 0 {
			flipped = append(flipped, it)
			continue
		}
		keptTop = append(keptTop, it)
	}
	for _, it := range low {
		if v, ok := freshDelta(it); ok && v >= 0 {
			flipped = append(flipped, it)
			continue
		}
		keptLow = append(keptLow, it)
	}
	for _, it := range flipped {
		if v, _ := freshDelta(it); v >= 0 {
			if !inBoard(keptTop, it.Symbol) {
				keptTop = append(keptTop, it)
			}
		} else {
			if !inBoard(keptLow, it.Symbol) {
				keptLow = append(keptLow, it)
			}
		}
	}
	return keptTop, keptLow
}

func formatPriceRankingZH(data *PriceRankingData, interesting map[string]bool, fresh60m map[string]float64) string {
	var sb strings.Builder

	sb.WriteString("## 涨跌幅排行\n\n")
	sb.WriteString("完整数据行 = 与候选池/持仓有交集的标的(其 1h 涨幅用本周期实时值覆盖);其余压缩为前3名概览(榜单为最多5分钟前的快照)。无对应列或 - 表示该数据不可用。\n\n")

	durationOrder := []string{"1h", "4h", "24h"}
	for _, duration := range durationOrder {
		durationData, exists := data.Durations[duration]
		if !exists || durationData == nil {
			continue
		}

		sb.WriteString(fmt.Sprintf("### %s 涨跌幅\n\n", duration))

		// Columns with no real data are dropped entirely — all-zero 资金流/OI
		// columns read as "nothing changed" and mislead the model.
		hasFlow, hasOI := rankingHasFlow(durationData), rankingHasOI(durationData)
		top, low := reclassifyFreshFlips(durationData.Top, durationData.Low, fresh60m, duration)

		if len(top) > 0 {
			sb.WriteString("**涨幅榜**\n")
			writePriceSide(&sb, top, hasFlow, hasOI, interesting, fresh60m, duration, "其余前列")
			sb.WriteString("\n")
		}

		if len(low) > 0 {
			sb.WriteString("**跌幅榜**\n")
			writePriceSide(&sb, low, hasFlow, hasOI, interesting, fresh60m, duration, "其余前列")
			sb.WriteString("\n")
		}
	}

	sb.WriteString("**解读**: 涨幅大+资金流入+OI增加=强势上涨 | 跌幅大+资金流出+OI减少=弱势下跌\n\n")
	return sb.String()
}

func rankingHasFlow(d *PriceRankingDuration) bool {
	for _, it := range append(append([]PriceRankingItem{}, d.Top...), d.Low...) {
		if it.FutureFlow != 0 || it.SpotFlow != 0 {
			return true
		}
	}
	return false
}

func rankingHasOI(d *PriceRankingDuration) bool {
	for _, it := range append(append([]PriceRankingItem{}, d.Top...), d.Low...) {
		if it.OIDeltaValue != 0 || it.OIDelta != 0 {
			return true
		}
	}
	return false
}

func rankingHeaderZH(hasFlow, hasOI bool) string {
	header := "| 币种 | 涨幅 | 价格 |"
	sep := "|------|------|------|"
	if hasFlow {
		header += " 资金流 |"
		sep += "--------|"
	}
	if hasOI {
		header += " OI变化 |"
		sep += "--------|"
	}
	return header + "\n" + sep + "\n"
}

func rankingRowZH(item PriceRankingItem, hasFlow, hasOI bool, fresh60m map[string]float64, duration string) string {
	delta := item.PriceDelta * 100
	if duration == "1h" {
		if fresh, ok := fresh60m[item.Symbol]; ok {
			delta = fresh
		}
	}
	row := fmt.Sprintf("| %s | %+.2f%% | $%.4f |", item.Symbol, delta, item.Price)
	if hasFlow {
		row += fmt.Sprintf(" %s |", formatValue(item.FutureFlow))
	}
	if hasOI {
		if item.OIDeltaValue == 0 && item.OIDelta == 0 {
			row += " — |" // no data for this row — never render a fake 0.00
		} else {
			row += fmt.Sprintf(" %s |", formatValue(item.OIDeltaValue))
		}
	}
	return row + "\n"
}

func formatPriceRankingEN(data *PriceRankingData, interesting map[string]bool, fresh60m map[string]float64) string {
	var sb strings.Builder

	sb.WriteString("## Price Gainers/Losers\n\n")
	sb.WriteString("Full rows = symbols overlapping the candidate pool / open positions (their 1h change uses this cycle live value); the rest collapse to a top-3 headline (snapshot up to 5 minutes old). Missing column or - = data unavailable.\n\n")

	durationOrder := []string{"1h", "4h", "24h"}
	for _, duration := range durationOrder {
		durationData, exists := data.Durations[duration]
		if !exists || durationData == nil {
			continue
		}

		sb.WriteString(fmt.Sprintf("### %s Price Change\n\n", duration))

		hasFlow, hasOI := rankingHasFlow(durationData), rankingHasOI(durationData)
		top, low := reclassifyFreshFlips(durationData.Top, durationData.Low, fresh60m, duration)

		if len(top) > 0 {
			sb.WriteString("**Top Gainers**\n")
			writePriceSideEN(&sb, top, hasFlow, hasOI, interesting, fresh60m, duration)
			sb.WriteString("\n")
		}

		if len(low) > 0 {
			sb.WriteString("**Top Losers**\n")
			writePriceSideEN(&sb, low, hasFlow, hasOI, interesting, fresh60m, duration)
			sb.WriteString("\n")
		}
	}

	sb.WriteString("**Note**: large gain + inflow + rising OI = strong uptrend; large drop + outflow + falling OI = weak downtrend.\n\n")
	return sb.String()
}

func writePriceSideEN(sb *strings.Builder, items []PriceRankingItem, hasFlow, hasOI bool, interesting map[string]bool, fresh60m map[string]float64, duration string) {
	// Two passes: table first, headline after.
	var fullRows, rest []PriceRankingItem
	for _, item := range items {
		if priceInteresting(item, interesting) {
			fullRows = append(fullRows, item)
		} else {
			rest = append(rest, item)
		}
	}
	if len(fullRows) > 0 {
		sb.WriteString("| Symbol | Change | Price |")
		sep := "|--------|--------|-------|"
		if hasFlow {
			sb.WriteString(" Fund Flow |")
			sep += "-----------|"
		}
		if hasOI {
			sb.WriteString(" OI Change |")
			sep += "-----------|"
		}
		sb.WriteString("\n" + sep + "\n")
		for _, item := range fullRows {
			sb.WriteString(rankingRowEN(item, hasFlow, hasOI, fresh60m, duration))
		}
	}
	if len(rest) > 0 {
		sb.WriteString("Others in the lead: ")
		for i, item := range rest {
			if i >= 3 {
				break
			}
			if i > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString(fmt.Sprintf("%s(%+.1f%%)", item.Symbol, item.PriceDelta*100))
		}
		sb.WriteString("\n")
	}
}

func rankingRowEN(item PriceRankingItem, hasFlow, hasOI bool, fresh60m map[string]float64, duration string) string {
	delta := item.PriceDelta * 100
	if duration == "1h" {
		if fresh, ok := fresh60m[item.Symbol]; ok {
			delta = fresh
		}
	}
	row := fmt.Sprintf("| %s | %+.2f%% | $%.4f |", item.Symbol, delta, item.Price)
	if hasFlow {
		row += fmt.Sprintf(" %s |", formatValue(item.FutureFlow))
	}
	if hasOI {
		if item.OIDeltaValue == 0 && item.OIDelta == 0 {
			row += " - |"
		} else {
			row += fmt.Sprintf(" %s |", formatValue(item.OIDeltaValue))
		}
	}
	return row + "\n"
}
