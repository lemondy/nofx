package kernel

import (
	"encoding/json"
	"fmt"
	"nofx/logger"
	"nofx/market"
	"nofx/mcp"
	"nofx/provider/nofxos"
	"nofx/store"
	"regexp"
	"strings"
	"time"
)

// ============================================================================
// Pre-compiled regular expressions (performance optimization)
// ============================================================================

var (
	// Safe regex: precisely match ```json code blocks
	// The (?:\{.*?\}\s*,?\s*)* group allows empty decision arrays [] — a valid
	// "no trades this cycle" verdict from the model.
	reJSONFence      = regexp.MustCompile(`(?is)` + "```json\\s*(\\[\\s*(?:\\{.*?\\}\\s*,?\\s*)*\\])\\s*```")
	reJSONArray      = regexp.MustCompile(`(?is)\[\s*(?:\{.*?\}\s*,?\s*)*\]`)
	reArrayHead      = regexp.MustCompile(`^\[\s*(?:\{|\])`)
	reArrayOpenSpace = regexp.MustCompile(`^\[\s+\{`)
	reInvisibleRunes = regexp.MustCompile("[\u200B\u200C\u200D\uFEFF]")
	// Placeholder values models emit for unknown numbers (?, ??, ？, N/A, —)
	rePlaceholderVal = regexp.MustCompile(`:\s*("[^"]*")?[?？]+\s*|:\s*"(?:N/A|n/a|NA|—|–|TBD|unknown)"`)
	reTrailingComma  = regexp.MustCompile(`,\s*(\}|\])`)

	// XML tag extraction (supports any characters in reasoning chain)
	reReasoningTag = regexp.MustCompile(`(?s)<reasoning>(.*?)</reasoning>`)
	reDecisionTag  = regexp.MustCompile(`(?s)<decision>(.*?)</decision>`)
)

// ============================================================================
// Entry Functions - Main API
// ============================================================================

// GetFullDecision gets AI's complete trading decision (batch analysis of all coins and positions)
// Uses default strategy configuration - for production use GetFullDecisionWithStrategy with explicit config
func GetFullDecision(ctx *Context, mcpClient mcp.AIClient) (*FullDecision, error) {
	defaultConfig := store.GetDefaultStrategyConfig("en")
	engine := NewStrategyEngine(&defaultConfig)
	return GetFullDecisionWithStrategy(ctx, mcpClient, engine, "")
}

// GetFullDecisionWithStrategy uses StrategyEngine to get AI decision (unified prompt generation)
func GetFullDecisionWithStrategy(ctx *Context, mcpClient mcp.AIClient, engine *StrategyEngine, variant string) (*FullDecision, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is nil")
	}
	if engine == nil {
		defaultConfig := store.GetDefaultStrategyConfig("en")
		engine = NewStrategyEngine(&defaultConfig)
	}

	// Clamp strategy limits to prevent token overflow
	engineConfig := engine.GetConfig()
	engineConfig.ClampLimits()

	// Token estimation check — block if exceeding the specific model's context limit
	estimate := engineConfig.EstimateTokens()

	// Determine context limit for the specific model being used
	contextLimit := 131072 // safe default (strictest common limit)
	var providerName string
	if embedder, ok := mcpClient.(mcp.ClientEmbedder); ok {
		base := embedder.BaseClient()
		providerName = base.Provider
		contextLimit = store.GetContextLimitForClient(base.Provider, base.Model)
	}

	if estimate.Total > contextLimit {
		logger.Errorf("🚫 Token estimate %d exceeds %s context limit %d — blocking analysis",
			estimate.Total, providerName, contextLimit)
		return nil, fmt.Errorf("estimated %d tokens exceeds model context limit of %d; reduce coins, timeframes, or K-line count",
			estimate.Total, contextLimit)
	}
	if estimate.Total*100/contextLimit >= 80 {
		logger.Infof("⚠️  Token estimate %d — approaching %s context limit %d",
			estimate.Total, providerName, contextLimit)
	}

	// 1. Fetch market data using strategy config
	if len(ctx.MarketDataMap) == 0 {
		if err := fetchMarketDataWithStrategy(ctx, engine); err != nil {
			return nil, fmt.Errorf("failed to fetch market data: %w", err)
		}
		// Header time must not predate the data it heads: CurrentTime was
		// stamped at context build, BEFORE this fetch loop spent ~1min
		// collecting 9 coins' klines/derivatives (09-18 audit #4: header
		// 15:30:22 vs signal timestamps 15:31:36+ confused the model's
		// freshness reasoning). Re-stamp at fetch completion — every signal
		// timestamp is now ≤ the header.
		ctx.CurrentTime = time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	}

	// Ensure OITopDataMap is initialized
	if ctx.OITopDataMap == nil {
		ctx.OITopDataMap = make(map[string]*OITopData)
		// Vergex only (same source as the data page). NofxOS public keys were
		// deprecated server-side, so there is no fallback.
		var oiPositions []nofxos.OIPosition
		if oiRanking, err := engine.vergexClient.GetOIRanking("1h", 20); err == nil {
			oiPositions = oiRanking.TopPositions
		}
		if len(oiPositions) > 0 {
			for _, pos := range oiPositions {
				ctx.OITopDataMap[pos.Symbol] = &OITopData{
					Rank:              pos.Rank,
					OIDeltaPercent:    pos.OIDeltaPercent,
					OIDeltaValue:      pos.OIDeltaValue,
					PriceDeltaPercent: pos.PriceDeltaPercent,
				}
			}
		}
	}

	// 2. Build System Prompt using strategy engine
	riskConfig := engine.GetRiskControlConfig()
	systemPrompt := engine.BuildSystemPrompt(ctx.Account.TotalEquity, variant)

	// 3. Build User Prompt using strategy engine
	userPrompt := engine.BuildUserPrompt(ctx)

	// 3.5 Regime-level skip (09-19 audit): when EVERY candidate is
	// double-blocked by the hard gate (no allowed direction, no
	// exception-eligible path) AND there are no positions to manage, the
	// LLM call can only ever return a hold — spending the tokens and the
	// minutes to hear it is pure waste (the audited cycle: 80k chars for
	// one hold, with ZEC the single variable). Synthesize the wait
	// programmatically; the decision record still lands with full prompts.
	// Anything alive — one allowed direction, one exception-eligible coin,
	// one open position — makes the call as usual.
	if len(ctx.Positions) == 0 && len(ctx.CandidateCoins) > 0 {
		allBlocked := true
		for _, coin := range ctx.CandidateCoins {
			if gs, ok := ctx.GateStates[market.Normalize(coin.Symbol)]; ok && gs != nil && !gs.HardBlocked {
				allBlocked = false
				break
			}
		}
		if allBlocked {
			logger.Infof("⏭️  [Regime Skip] 全部 %d 个候选双向硬门拦截且无持仓 — 跳过本次 LLM 调用,程序合成 wait(下一周期快照自动重评)", len(ctx.CandidateCoins))
			fd := &FullDecision{
				Decisions: []Decision{{
					Symbol:    "ALL",
					Action:    "wait",
					Reasoning: fmt.Sprintf("Regime skip: 全部 %d 个候选的开仓硬门双向均为程序拦截(无 allowed 方向、无市价例外路径),且当前无持仓需要管理 — 程序直接合成 wait,本轮未调用 LLM;候选结构变化后下一周期快照自动重评", len(ctx.CandidateCoins)),
				}},
				SystemPrompt: systemPrompt,
				UserPrompt:   userPrompt,
				RawResponse:  "program-synthesized wait (regime skip)",
				Timestamp:    time.Now(),
			}
			return fd, nil
		}
	}

	// 4. Call AI API
	aiCallStart := time.Now()
	aiResponse, err := mcpClient.CallWithMessages(systemPrompt, userPrompt)
	aiCallDuration := time.Since(aiCallStart)
	if err != nil {
		return nil, fmt.Errorf("AI API call failed: %w", err)
	}

	// 5. Parse AI response
	decision, err := parseFullDecisionResponse(
		aiResponse,
		ctx.Account.TotalEquity,
		riskConfig.BTCETHMaxLeverage,
		riskConfig.AltcoinMaxLeverage,
		riskConfig.BTCETHMaxPositionValueRatio,
		riskConfig.AltcoinMaxPositionValueRatio,
		engine.EffectiveMinPositionSize(),
		positionSymbolsFromContext(ctx),
		ctx.GateStates,
	)

	if decision != nil {
		decision.Timestamp = time.Now()
		decision.SystemPrompt = systemPrompt
		decision.UserPrompt = userPrompt
		decision.AIRequestDurationMs = aiCallDuration.Milliseconds()
		decision.RawResponse = aiResponse
		// Anchor compliance: snap open_*_limit prices back to the
		// pre-computed values the prompt showed when the model drifted.
		if err == nil {
			correctLimitAnchors(decision.Decisions, ctx.LimitAnchors, LimitAnchorTolerancePct)
		}
	}

	if err != nil {
		return decision, fmt.Errorf("failed to parse AI response: %w", err)
	}

	return decision, nil
}

// ============================================================================
// Market Data Fetching
// ============================================================================

// fetchMarketDataWithStrategy fetches market data using strategy config (multiple timeframes)
func fetchMarketDataWithStrategy(ctx *Context, engine *StrategyEngine) error {
	config := engine.GetConfig()
	ctx.MarketDataMap = make(map[string]*market.Data)

	timeframes := config.Indicators.Klines.SelectedTimeframes
	primaryTimeframe := config.Indicators.Klines.PrimaryTimeframe
	klineCount := config.Indicators.Klines.PrimaryCount

	// Compatible with old configuration
	if len(timeframes) == 0 {
		if primaryTimeframe != "" {
			timeframes = append(timeframes, primaryTimeframe)
		} else {
			timeframes = append(timeframes, "3m")
		}
		if config.Indicators.Klines.LongerTimeframe != "" {
			timeframes = append(timeframes, config.Indicators.Klines.LongerTimeframe)
		}
	}
	if primaryTimeframe == "" {
		primaryTimeframe = timeframes[0]
	}
	if klineCount <= 0 {
		klineCount = 30
	}

	logger.Infof("📊 Strategy timeframes: %v, Primary: %s, Kline count: %d", timeframes, primaryTimeframe, klineCount)

	// 1. First fetch data for position coins (must fetch)
	for _, pos := range ctx.Positions {
		data, err := market.GetWithTimeframes(pos.Symbol, timeframes, primaryTimeframe, klineCount)
		if err != nil {
			logger.Infof("⚠️  Failed to fetch market data for position %s: %v", pos.Symbol, err)
			continue
		}
		ctx.MarketDataMap[pos.Symbol] = data
	}

	// 2. Fetch data for all candidate coins
	positionSymbols := make(map[string]bool)
	for _, pos := range ctx.Positions {
		positionSymbols[pos.Symbol] = true
	}

	// Minimum OI value filter — per-strategy config, 0/unset = built-in 15M USD.
	minOIThresholdMillions := config.CoinSource.EffectiveMinOIMillions()

	for _, coin := range ctx.CandidateCoins {
		if _, exists := ctx.MarketDataMap[coin.Symbol]; exists {
			continue
		}

		data, err := market.GetWithTimeframes(coin.Symbol, timeframes, primaryTimeframe, klineCount)
		if err != nil {
			logger.Infof("⚠️  Failed to fetch market data for %s: %v", coin.Symbol, err)
			continue
		}

		// Liquidity filter (skip for xyz dex assets - they don't have OI data from Binance)
		isExistingPosition := positionSymbols[coin.Symbol]
		isXyzAsset := market.IsXyzDexAsset(coin.Symbol)
		if !isExistingPosition && !isXyzAsset && data.OpenInterest != nil && data.CurrentPrice > 0 {
			oiValue := data.OpenInterest.Latest * data.CurrentPrice
			oiValueInMillions := oiValue / 1_000_000
			if oiValueInMillions < minOIThresholdMillions {
				logger.Infof("⚠️  %s OI value too low (%.2fM USD < %.1fM), skipping coin",
					coin.Symbol, oiValueInMillions, minOIThresholdMillions)
				continue
			}
		}

		// XYZ tokenized listings (data.Symbol carries the "NAME:TICKER"
		// exchange form) must never reach the decision layer — the candidate
		// name was normalized (colon stripped) so the choke-point filter
		// missed them; stop them here instead.
		if strings.Contains(strings.ToUpper(data.Symbol), ":") {
			logger.Infof("🚫 Excluded XYZ (tokenized) symbol from market data: %s (candidate %s)", data.Symbol, coin.Symbol)
			continue
		}
		ctx.MarketDataMap[coin.Symbol] = data
	}

	logger.Infof("📊 Successfully fetched multi-timeframe market data for %d coins", len(ctx.MarketDataMap))
	return nil
}

// ============================================================================
// AI Response Parsing
// ============================================================================

func parseFullDecisionResponse(aiResponse string, accountEquity float64, btcEthLeverage, altcoinLeverage int, btcEthPosRatio, altcoinPosRatio float64, minPositionSize float64, positionSymbols map[string]bool, gateStates map[string]*GateState) (*FullDecision, error) {
	cotTrace := extractCoTTrace(aiResponse)

	decisions, err := extractDecisions(aiResponse)
	if err != nil {
		return &FullDecision{
			CoTTrace:  cotTrace,
			Decisions: []Decision{},
		}, fmt.Errorf("failed to extract decisions: %w", err)
	}

	if err := validateDecisions(decisions, accountEquity, btcEthLeverage, altcoinLeverage, btcEthPosRatio, altcoinPosRatio, minPositionSize, positionSymbols, gateStates); err != nil {
		return &FullDecision{
			CoTTrace:  cotTrace,
			Decisions: decisions,
		}, fmt.Errorf("decision validation failed: %w", err)
	}

	return &FullDecision{
		CoTTrace:  cotTrace,
		Decisions: decisions,
	}, nil
}

func extractCoTTrace(response string) string {
	if match := reReasoningTag.FindStringSubmatch(response); match != nil && len(match) > 1 {
		logger.Infof("✓ Extracted reasoning chain using <reasoning> tag")
		return strings.TrimSpace(match[1])
	}

	if decisionIdx := strings.Index(response, "<decision>"); decisionIdx > 0 {
		logger.Infof("✓ Extracted content before <decision> tag as reasoning chain")
		return strings.TrimSpace(response[:decisionIdx])
	}

	jsonStart := strings.Index(response, "[")
	if jsonStart > 0 {
		logger.Infof("⚠️  Extracted reasoning chain using old format ([ character separator)")
		return strings.TrimSpace(response[:jsonStart])
	}

	return strings.TrimSpace(response)
}

func extractDecisions(response string) ([]Decision, error) {
	s := removeInvisibleRunes(response)
	s = sanitizePlaceholders(s)
	s = strings.TrimSpace(s)
	s = fixMissingQuotes(s)

	var jsonPart string
	if match := reDecisionTag.FindStringSubmatch(s); match != nil && len(match) > 1 {
		jsonPart = strings.TrimSpace(match[1])
		logger.Infof("✓ Extracted JSON using <decision> tag")
	} else {
		jsonPart = s
		logger.Infof("⚠️  <decision> tag not found, searching JSON in full text")
	}

	jsonPart = fixMissingQuotes(jsonPart)

	if m := reJSONFence.FindStringSubmatch(jsonPart); m != nil && len(m) > 1 {
		jsonContent := strings.TrimSpace(m[1])
		jsonContent = compactArrayOpen(jsonContent)
		jsonContent = fixMissingQuotes(jsonContent)
		if err := validateJSONFormat(jsonContent); err != nil {
			return nil, fmt.Errorf("JSON format validation failed: %w\nJSON content: %s\nFull response:\n%s", err, jsonContent, response)
		}
		var decisions []Decision
		if err := json.Unmarshal([]byte(jsonContent), &decisions); err != nil {
			return nil, fmt.Errorf("JSON parsing failed: %w\nJSON content: %s", err, jsonContent)
		}
		return decisions, nil
	}

	jsonContent := strings.TrimSpace(reJSONArray.FindString(jsonPart))
	if jsonContent == "" {
		// Max-token truncation amputates the tail of the decision array: the
		// model has usually already emitted several complete decision objects
		// before the cut. Salvage them instead of discarding the whole
		// response to safe-wait — the earlier coins' decisions are real
		// analysis, and a truncated WAIT-heavy board is still directionally
		// correct (positions beyond the cut just keep their default behavior).
		if salvaged := salvageTruncatedDecisionArray(jsonPart); salvaged != nil {
			logger.Warnf("⚠️  [TruncatedSalvage] Decision array was cut off mid-stream (max_tokens); recovered %d complete decisions, later coins default to no-action", len(salvaged))
			return salvaged, nil
		}

		if strings.TrimSpace(jsonPart) == "" {
			logger.Warnf("⚠️  [SafeFallback] AI response is empty after cleanup (%d raw bytes) — upstream returned blank content (check truncation/limits)", len(response))
		} else {
			logger.Infof("⚠️  [SafeFallback] AI didn't output JSON decision, entering safe wait mode")
		}

		// The model's conclusion sits at the END of its reasoning (the beginning
		// is preamble like "Let me analyze this carefully...") — summarize the
		// tail so users see the actual take-away.
		cotSummary := summarizeTail(jsonPart, 240)

		fallbackDecision := Decision{
			Symbol:    "ALL",
			Action:    "wait",
			Reasoning: fmt.Sprintf("Model didn't output structured JSON decision, entering safe wait; summary: %s", cotSummary),
		}

		return []Decision{fallbackDecision}, nil
	}

	jsonContent = compactArrayOpen(jsonContent)
	jsonContent = fixMissingQuotes(jsonContent)

	if err := validateJSONFormat(jsonContent); err != nil {
		return nil, fmt.Errorf("JSON format validation failed: %w\nJSON content: %s\nFull response:\n%s", err, jsonContent, response)
	}

	var decisions []Decision
	if err := json.Unmarshal([]byte(jsonContent), &decisions); err != nil {
		return nil, fmt.Errorf("JSON parsing failed: %w\nJSON content: %s", err, jsonContent)
	}

	return decisions, nil
}

// sanitizePlaceholders replaces placeholder values models emit for unknown
// numbers (?, ??, full-width ？, "N/A", "—") with null, which json.Unmarshal
// treats as "leave the field at zero" — the decision stays executable instead
// of being rejected wholesale by strict JSON parsing.
func sanitizePlaceholders(s string) string {
	s = rePlaceholderVal.ReplaceAllString(s, ": null")
	s = reTrailingComma.ReplaceAllString(s, "$1")
	return s
}

func fixMissingQuotes(jsonStr string) string {
	jsonStr = strings.ReplaceAll(jsonStr, "\u201c", "\"")
	jsonStr = strings.ReplaceAll(jsonStr, "\u201d", "\"")
	jsonStr = strings.ReplaceAll(jsonStr, "\u2018", "'")
	jsonStr = strings.ReplaceAll(jsonStr, "\u2019", "'")

	jsonStr = strings.ReplaceAll(jsonStr, "［", "[")
	jsonStr = strings.ReplaceAll(jsonStr, "］", "]")
	jsonStr = strings.ReplaceAll(jsonStr, "｛", "{")
	jsonStr = strings.ReplaceAll(jsonStr, "｝", "}")
	jsonStr = strings.ReplaceAll(jsonStr, "：", ":")
	jsonStr = strings.ReplaceAll(jsonStr, "，", ",")

	jsonStr = strings.ReplaceAll(jsonStr, "【", "[")
	jsonStr = strings.ReplaceAll(jsonStr, "】", "]")
	jsonStr = strings.ReplaceAll(jsonStr, "〔", "[")
	jsonStr = strings.ReplaceAll(jsonStr, "〕", "]")
	jsonStr = strings.ReplaceAll(jsonStr, "、", ",")

	jsonStr = strings.ReplaceAll(jsonStr, "　", " ")

	return jsonStr
}

func validateJSONFormat(jsonStr string) error {
	trimmed := strings.TrimSpace(jsonStr)

	// An empty decision array [] is a valid "no trades this cycle" verdict.
	if trimmed == "[]" {
		return nil
	}

	if !reArrayHead.MatchString(trimmed) {
		if strings.HasPrefix(trimmed, "[") && !strings.Contains(trimmed[:min(20, len(trimmed))], "{") {
			return fmt.Errorf("not a valid decision array (must contain objects {}), actual content: %s", trimmed[:min(50, len(trimmed))])
		}
		return fmt.Errorf("JSON must start with [{ (whitespace allowed), actual: %s", trimmed[:min(20, len(trimmed))])
	}

	if strings.Contains(jsonStr, "~") {
		return fmt.Errorf("JSON cannot contain range symbol ~, all numbers must be precise single values")
	}

	for i := 0; i < len(jsonStr)-4; i++ {
		if jsonStr[i] >= '0' && jsonStr[i] <= '9' &&
			jsonStr[i+1] == ',' &&
			jsonStr[i+2] >= '0' && jsonStr[i+2] <= '9' &&
			jsonStr[i+3] >= '0' && jsonStr[i+3] <= '9' &&
			jsonStr[i+4] >= '0' && jsonStr[i+4] <= '9' {
			return fmt.Errorf("JSON numbers cannot contain thousand separator comma, found: %s", jsonStr[i:min(i+10, len(jsonStr))])
		}
	}

	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func removeInvisibleRunes(s string) string {
	return reInvisibleRunes.ReplaceAllString(s, "")
}

func compactArrayOpen(s string) string {
	return reArrayOpenSpace.ReplaceAllString(strings.TrimSpace(s), "[{")
}

// salvageTruncatedDecisionArray recovers complete decision objects from a
// response whose closing "]" was amputated by max-token truncation. It scans
// the first JSON-looking array, tracks brace depth (string-aware, escape-safe),
// and cuts after the last object that closed at depth 1 — then re-arms the
// array and parses normally. Returns nil when nothing complete survives.
func salvageTruncatedDecisionArray(s string) []Decision {
	start := strings.Index(s, "[")
	if start < 0 {
		return nil
	}
	depth, lastComplete := 0, -1
	inString, escaped := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				lastComplete = i
			} else if depth < 0 {
				return nil // stray close brace — not a salvageable array
			}
		}
	}
	if lastComplete < 0 {
		return nil
	}
	candidate := s[start:lastComplete+1] + "]"
	if err := validateJSONFormat(candidate); err != nil {
		return nil
	}
	var decisions []Decision
	if err := json.Unmarshal([]byte(candidate), &decisions); err != nil {
		return nil
	}
	return decisions
}

// summarizeTail returns the last n characters of s (with an ellipsis prefix
// when truncated) — reasoning models put their conclusion at the end.
func summarizeTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// positionSymbolsFromContext builds the open-position symbol set the stage
// derivation reads (hold: IN_POSITION vs NO_SETUP). Symbols are normalized
// so "NEAR" and "NEARUSDT" forms match.
func positionSymbolsFromContext(ctx *Context) map[string]bool {
	set := map[string]bool{}
	if ctx == nil {
		return set
	}
	for _, p := range ctx.Positions {
		if p.Symbol != "" {
			set[market.Normalize(p.Symbol)] = true
		}
	}
	return set
}
