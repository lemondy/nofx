package nofxos

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// OIPosition represents open interest data for a single coin
type OIPosition struct {
	Symbol            string  `json:"symbol"`
	Rank              int     `json:"rank"`
	Price             float64 `json:"price"`
	CurrentOI         float64 `json:"current_oi"`
	OIDelta           float64 `json:"oi_delta"`
	OIDeltaPercent    float64 `json:"oi_delta_percent"`    // Already x100 (5.0 = 5%)
	OIDeltaValue      float64 `json:"oi_delta_value"`      // USDT value
	PriceDeltaPercent float64 `json:"price_delta_percent"` // Already x100 (5.0 = 5%)
	NetLong           float64 `json:"net_long"`
	NetShort          float64 `json:"net_short"`
}

// OIRankingResponse is the API response structure for OI ranking
type OIRankingResponse struct {
	Success bool `json:"success"`
	Code    int  `json:"code"`
	Data    struct {
		Positions      []OIPosition `json:"positions"`
		Count          int          `json:"count"`
		Exchange       string       `json:"exchange"`
		TimeRange      string       `json:"time_range"`
		TimeRangeParam string       `json:"time_range_param"`
		RankType       string       `json:"rank_type"`
		Limit          int          `json:"limit"`
	} `json:"data"`
}

// OIRankingData contains both top and low OI rankings
type OIRankingData struct {
	TimeRange    string       `json:"time_range"`
	Duration     string       `json:"duration"`
	TopPositions []OIPosition `json:"top_positions"`
	LowPositions []OIPosition `json:"low_positions"`
	FetchedAt    time.Time    `json:"fetched_at"`
}

// GetOIRanking retrieves OI ranking data (both top increase and low decrease)
func (c *Client) GetOIRanking(duration string, limit int) (*OIRankingData, error) {
	if duration == "" {
		duration = "1h"
	}
	if limit <= 0 {
		limit = 20
	}

	result := &OIRankingData{
		Duration:  duration,
		FetchedAt: time.Now(),
	}

	// Fetch top ranking (OI increase)
	topPositions, timeRange, err := c.fetchOIRanking("top", duration, limit)
	if err != nil {
		log.Printf("⚠️  Failed to fetch OI top ranking: %v", err)
	} else {
		result.TopPositions = topPositions
		result.TimeRange = timeRange
	}

	// Fetch low ranking (OI decrease)
	lowPositions, _, err := c.fetchOIRanking("low", duration, limit)
	if err != nil {
		log.Printf("⚠️  Failed to fetch OI low ranking: %v", err)
	} else {
		result.LowPositions = lowPositions
	}

	log.Printf("✓ Fetched OI ranking data: %d top, %d low (duration: %s)",
		len(result.TopPositions), len(result.LowPositions), duration)

	return result, nil
}

func (c *Client) fetchOIRanking(rankType, duration string, limit int) ([]OIPosition, string, error) {
	endpoint := fmt.Sprintf("/api/oi/%s-ranking?limit=%d&duration=%s", rankType, limit, duration)

	body, err := c.doRequest(endpoint)
	if err != nil {
		return nil, "", fmt.Errorf("request failed: %w", err)
	}

	var response OIRankingResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, "", fmt.Errorf("JSON parsing failed: %w", err)
	}

	// Check for success (support both success field and code field)
	if !response.Success && response.Code != 0 {
		return nil, "", fmt.Errorf("API returned error code: %d", response.Code)
	}

	return response.Data.Positions, response.Data.TimeRange, nil
}

// GetOITopPositions retrieves top OI increase positions (legacy compatibility)
func (c *Client) GetOITopPositions() ([]OIPosition, error) {
	positions, _, err := c.fetchOIRanking("top", "1h", 20)
	if err != nil {
		return nil, err
	}
	return positions, nil
}

// GetOITopSymbols retrieves OI top coin symbol list
func (c *Client) GetOITopSymbols() ([]string, error) {
	positions, err := c.GetOITopPositions()
	if err != nil {
		return nil, err
	}

	var symbols []string
	for _, pos := range positions {
		symbol := NormalizeSymbol(pos.Symbol)
		symbols = append(symbols, symbol)
	}

	return symbols, nil
}

// GetOILowPositions retrieves OI decrease positions (for short opportunities)
func (c *Client) GetOILowPositions() ([]OIPosition, error) {
	positions, _, err := c.fetchOIRanking("low", "1h", 20)
	if err != nil {
		return nil, err
	}
	return positions, nil
}

// GetOILowSymbols retrieves OI low coin symbol list
func (c *Client) GetOILowSymbols() ([]string, error) {
	positions, err := c.GetOILowPositions()
	if err != nil {
		return nil, err
	}

	var symbols []string
	for _, pos := range positions {
		symbol := NormalizeSymbol(pos.Symbol)
		symbols = append(symbols, symbol)
	}

	return symbols, nil
}

// FormatOIRankingForAI formats OI ranking data for AI consumption.
// interesting marks symbols worth full rows (candidates/open positions);
// everything else collapses to a compact headline so the table doesn't eat
// the context budget.
func FormatOIRankingForAI(data *OIRankingData, lang Language, interesting map[string]bool) string {
	if data == nil {
		return ""
	}

	if lang == LangChinese {
		return formatOIRankingZH(data, interesting)
	}
	return formatOIRankingEN(data, interesting)
}

// oiInterestingRow decides whether a position deserves a full row.
func oiInterestingRow(pos OIPosition, interesting map[string]bool) bool {
	return interesting[pos.Symbol]
}

func formatOIRankingZH(data *OIRankingData, interesting map[string]bool) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## 持仓量变化排行 (%s)\n\n", data.Duration))
	sb.WriteString("完整数据行 = 与候选池/持仓有交集的标的;其余压缩为前3名概览。\n\n")

	writeSide := func(title string, positions []OIPosition) {
		if len(positions) == 0 {
			return
		}
		sb.WriteString(fmt.Sprintf("### %s\n", title))
		// Two passes: full table first, headline after — a single pass lets
		// "其余前列:" text land mid-table and breaks the markdown.
		var fullRows, rest []OIPosition
		for _, pos := range positions {
			if oiInterestingRow(pos, interesting) {
				fullRows = append(fullRows, pos)
			} else {
				rest = append(rest, pos)
			}
		}
		if len(fullRows) > 0 {
			sb.WriteString("| 排名 | 币种 | OI变化(USDT) | OI变化% | 价格变化% |\n")
			sb.WriteString("|------|------|----------------|---------|----------|\n")
			for _, pos := range fullRows {
				sb.WriteString(fmt.Sprintf("| %d | %s | %s | %+.2f%% | %+.2f%% | ← 候选/持仓\n",
					pos.Rank, pos.Symbol, formatValue(pos.OIDeltaValue),
					pos.OIDeltaPercent, pos.PriceDeltaPercent))
			}
		}
		if len(rest) > 0 {
			sb.WriteString("其余前列: ")
			for i, pos := range rest {
				if i >= 3 {
					break
				}
				if i > 0 {
					sb.WriteString(" ")
				}
				sb.WriteString(fmt.Sprintf("%s(%+.1f%%)", pos.Symbol, pos.OIDeltaPercent))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	writeSide("持仓增加榜", data.TopPositions)
	writeSide("持仓减少榜", data.LowPositions)

	sb.WriteString("**解读**: OI增+价涨=多头主导 | OI增+价跌=空头主导 | OI减+价涨=空头平仓 | OI减+价跌=多头平仓\n\n")
	return sb.String()
}

func formatOIRankingEN(data *OIRankingData, interesting map[string]bool) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Open Interest Ranking (%s)\n\n", data.Duration))
	sb.WriteString("Full rows = symbols overlapping the candidate pool / open positions; the rest collapse to a top-3 headline.\n\n")

	writeSide := func(title string, positions []OIPosition) {
		if len(positions) == 0 {
			return
		}
		sb.WriteString(fmt.Sprintf("### %s\n", title))
		// Two passes: table first, headline after (single-pass interleaving
		// breaks the markdown when an interesting row follows 3 others).
		var fullRows, rest []OIPosition
		for _, pos := range positions {
			if oiInterestingRow(pos, interesting) {
				fullRows = append(fullRows, pos)
			} else {
				rest = append(rest, pos)
			}
		}
		if len(fullRows) > 0 {
			sb.WriteString("| Rank | Symbol | OI Change (USD) | OI Change% | Price Change% |\n")
			sb.WriteString("|------|--------|------------------|------------|---------------|\n")
			for _, pos := range fullRows {
				sb.WriteString(fmt.Sprintf("| %d | %s | %s | %+.2f%% | %+.2f%% | ← candidate/position\n",
					pos.Rank, pos.Symbol, formatValue(pos.OIDeltaValue),
					pos.OIDeltaPercent, pos.PriceDeltaPercent))
			}
		}
		if len(rest) > 0 {
			sb.WriteString("Others in the lead: ")
			for i, pos := range rest {
				if i >= 3 {
					break
				}
				if i > 0 {
					sb.WriteString(" ")
				}
				sb.WriteString(fmt.Sprintf("%s(%+.1f%%)", pos.Symbol, pos.OIDeltaPercent))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	writeSide("OI Increasing", data.TopPositions)
	writeSide("OI Decreasing", data.LowPositions)

	sb.WriteString("**Reading**: OI up + price up = longs leading | OI up + price down = shorts leading | OI down + price up = shorts closing | OI down + price down = longs closing\n\n")
	return sb.String()
}
