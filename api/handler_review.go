package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/mcp"
	"nofx/store"
)

// getJournalTrader resolves trader_id from query and validates ownership
func (s *Server) getJournalTrader(c *gin.Context) (string, bool) {
	_, traderID, err := s.getTraderFromQuery(c)
	if err != nil || traderID == "" {
		SafeBadRequest(c, "Invalid trader ID")
		return "", false
	}
	return traderID, true
}

// handleJournalList GET /api/review/journal
// Query: ?trader_id=&limit=&offset=&symbol=&review_status=
func (s *Server) handleJournalList(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	// Classify legacy rows on first read after deploy (NULL ai_managed) —
	// idempotent, no-op when everything is stamped (user request 09-27).
	if n, err := s.store.TradeJournal().BackfillOwnership(traderID); err != nil {
		logger.Warnf("⚠️ journal ownership backfill failed (rendering continues without badges): %v", err)
	} else if n > 0 {
		logger.Infof("🤖 journal ownership backfilled: %d rows classified (AI vs manual)", n)
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))

	entries, total, err := s.store.TradeJournal().List(traderID, limit, offset,
		c.Query("symbol"), c.Query("review_status"))
	if err != nil {
		SafeInternalError(c, "list trade journal", err)
		return
	}
	if entries == nil {
		entries = []*store.TradeJournalDB{}
	}
	c.JSON(http.StatusOK, gin.H{
		"entries": entries,
		"total":   total,
	})
}

// handleJournalUpdate POST /api/review/journal/:id
// Body: review fields (executed_as_plan, deviation_note, emotions, mistake_category, strategy_tag, lesson)
func (s *Server) handleJournalUpdate(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		SafeBadRequest(c, "Invalid journal entry ID")
		return
	}

	var body store.JournalReviewPayload
	if err := c.ShouldBindJSON(&body); err != nil {
		SafeBadRequest(c, "Invalid request body")
		return
	}

	// Validate enum-ish fields
	if body.ExecutedAsPlan != nil {
		switch *body.ExecutedAsPlan {
		case "yes", "partial", "no", "":
		default:
			SafeBadRequest(c, "executed_as_plan must be one of: yes, partial, no")
			return
		}
	}
	if body.MistakeCategory != nil {
		switch *body.MistakeCategory {
		case "strategy", "execution", "risk_control", "market", "none", "":
		default:
			SafeBadRequest(c, "mistake_category must be one of: strategy, execution, risk_control, market, none")
			return
		}
	}

	entry, err := s.store.TradeJournal().UpdateReviewPayload(traderID, id, &body)
	if err != nil {
		SafeInternalError(c, "update journal entry", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"entry": entry})
}

// handleJournalSync POST /api/review/journal/sync
// Force-sync journal entries from closed positions
func (s *Server) handleJournalSync(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	created, err := s.store.TradeJournal().SyncFromPositions(traderID)
	if err != nil {
		SafeInternalError(c, "sync trade journal", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"created": created})
}

// handleJournalStats GET /api/review/journal/stats
// Aggregated review statistics (win rate / expectancy / adherence / emotions / categories)
func (s *Server) handleJournalStats(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	stats, err := s.store.TradeJournal().GetStats(traderID)
	if err != nil {
		SafeInternalError(c, "journal statistics", err)
		return
	}
	c.JSON(http.StatusOK, stats)
}

// handleRulesList GET /api/review/rules
func (s *Server) handleRulesList(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	rules, err := s.store.Rule().ListRules(traderID)
	if err != nil {
		SafeInternalError(c, "list trading rules", err)
		return
	}
	if rules == nil {
		rules = []*store.TradingRuleDB{}
	}
	c.JSON(http.StatusOK, gin.H{"rules": s.annotateRules(traderID, rules, time.Now().UTC().UnixMilli())})
}

// ruleView a rule plus computed staleness fields (review 2026-10-09 K).
type ruleView struct {
	*store.TradingRuleDB
	Triggers30d     int64 `json:"triggers_30d"`
	LastTriggeredAt int64 `json:"last_triggered_at"`
	ReviewDue       bool  `json:"review_due"`
}

const ruleReviewAgeMs = int64(30) * 24 * 3600 * 1000

// annotateRules adds triggers_30d / last_triggered_at / review_due using one
// grouped query over rule_check_logs. review_due: AI-sourced rule older than
// 30 days whose verification is older than 30 days (or never done).
func (s *Server) annotateRules(traderID string, rules []*store.TradingRuleDB, nowMs int64) []ruleView {
	stats, err := s.store.Rule().TriggerStats(traderID, nowMs-ruleReviewAgeMs)
	if err != nil {
		logger.Warnf("rule trigger stats failed (rendering without counts): %v", err)
	}
	out := make([]ruleView, 0, len(rules))
	for _, r := range rules {
		v := ruleView{TradingRuleDB: r}
		if st, ok := stats[r.ID]; ok {
			v.Triggers30d, v.LastTriggeredAt = st.Triggers, st.LastAtMs
		}
		v.ReviewDue = r.Source == "ai_review" && r.CreatedAt > 0 && nowMs-r.CreatedAt > ruleReviewAgeMs &&
			(r.VerifiedAt == 0 || nowMs-r.VerifiedAt > ruleReviewAgeMs)
		out = append(out, v)
	}
	return out
}

// verifyRule replays a rule over the trader's closed AI trades of the last
// 90 days (all of them, not only the reviewed subset — review 2026-10-09 K).
func (s *Server) verifyRule(userID, traderID, ruleType, condition string) (kernel.RuleVerification, error) {
	if ruleType == "soft" {
		return kernel.VerifyHardRule(ruleType, condition, nil, 0, 0), nil
	}
	nowMs := time.Now().UTC().UnixMilli()
	since := nowMs - int64(kernel.RuleVerifyWindowDays)*24*3600*1000
	trades, err := s.store.TradeJournal().ListClosedAISince(traderID, since)
	if err != nil {
		return kernel.RuleVerification{}, err
	}
	equity := 0.0
	if tr, err := s.store.Trader().GetByID(traderID); err == nil && tr != nil {
		equity = tr.InitialBalance
	}
	return kernel.VerifyHardRule(ruleType, condition, trades, equity, nowMs), nil
}

// handleRuleReverify POST /api/review/rules/:id/reverify — recompute and store
// the verification of one rule.
func (s *Server) handleRuleReverify(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		SafeBadRequest(c, "Invalid rule ID")
		return
	}
	rule, err := s.store.Rule().GetRule(traderID, id)
	if err != nil {
		SafeBadRequest(c, "Rule not found")
		return
	}
	v, err := s.verifyRule(c.GetString("user_id"), traderID, rule.RuleType, rule.ConditionJSON)
	if err != nil {
		SafeInternalError(c, "verify trading rule", err)
		return
	}
	data, _ := json.Marshal(v)
	now := time.Now().UTC().UnixMilli()
	if err := s.store.Rule().SetVerification(traderID, id, string(data), now); err != nil {
		SafeInternalError(c, "store rule verification", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"verification": v, "verified_at": now})
}

// handleRuleCreate POST /api/review/rules
func (s *Server) handleRuleCreate(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	var input store.RuleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		SafeBadRequest(c, "Invalid request body")
		return
	}
	if err := validateRuleInput(&input, true); err != nil {
		SafeBadRequest(c, err.Error())
		return
	}
	rule, err := s.store.Rule().CreateRule(traderID, &input)
	if err != nil {
		SafeInternalError(c, "create trading rule", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"rule": rule})
}

// handleRuleUpdate POST /api/review/rules/:id
func (s *Server) handleRuleUpdate(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		SafeBadRequest(c, "Invalid rule ID")
		return
	}
	var input store.RuleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		SafeBadRequest(c, "Invalid request body")
		return
	}
	if err := validateRuleInput(&input, false); err != nil {
		SafeBadRequest(c, err.Error())
		return
	}
	rule, err := s.store.Rule().UpdateRule(traderID, id, &input)
	if err != nil {
		SafeInternalError(c, "update trading rule", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"rule": rule})
}

// handleRuleDelete DELETE /api/review/rules/:id
func (s *Server) handleRuleDelete(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		SafeBadRequest(c, "Invalid rule ID")
		return
	}
	if err := s.store.Rule().DeleteRule(traderID, id); err != nil {
		SafeInternalError(c, "delete trading rule", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// validateRuleInput validates rule create/update payloads
func validateRuleInput(input *store.RuleInput, creating bool) error {
	if creating && strings.TrimSpace(input.Name) == "" {
		return fmt.Errorf("rule name is required")
	}
	switch input.RuleType {
	case "hard":
		if creating {
			if strings.TrimSpace(input.Condition) == "" {
				return fmt.Errorf("hard rule requires a condition")
			}
			if _, err := kernel.ParseRuleCondition(input.Condition); err != nil {
				return err
			}
		} else if input.Condition != "" {
			if _, err := kernel.ParseRuleCondition(input.Condition); err != nil {
				return err
			}
		}
	case "soft":
		if creating && strings.TrimSpace(input.LessonText) == "" {
			return fmt.Errorf("soft rule requires lesson_text")
		}
	case "":
		if creating {
			return fmt.Errorf("rule_type is required")
		}
	default:
		return fmt.Errorf("rule_type must be hard or soft")
	}
	switch input.OnViolation {
	case "block", "warn", "":
	default:
		return fmt.Errorf("on_violation must be block or warn")
	}
	return nil
}

// handleRuleCheckLogs GET /api/review/rules/logs
func (s *Server) handleRuleCheckLogs(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	logs, err := s.store.Rule().ListCheckLogs(traderID, limit)
	if err != nil {
		SafeInternalError(c, "list rule check logs", err)
		return
	}
	if logs == nil {
		logs = []*store.RuleCheckLogDB{}
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}

// handleRuleCheck POST /api/review/rules/check
// Dry-run a hypothetical decision against the rule system (pre-trade check UI)
func (s *Server) handleRuleCheck(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	var req struct {
		Symbol          string  `json:"symbol"`
		Action          string  `json:"action"` // open_long|open_short
		Leverage        int     `json:"leverage"`
		PositionSizeUSD float64 `json:"position_size_usd"`
		StopLoss        float64 `json:"stop_loss"`
		TakeProfit      float64 `json:"take_profit"`
		Confidence      int     `json:"confidence"`
		Price           float64 `json:"price"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		SafeBadRequest(c, "Invalid request body")
		return
	}
	if req.Action != "open_long" && req.Action != "open_short" {
		SafeBadRequest(c, "action must be open_long or open_short")
		return
	}
	if req.Symbol == "" {
		SafeBadRequest(c, "symbol is required")
		return
	}

	rules, err := s.store.Rule().GetEnabledRules(traderID)
	if err != nil {
		SafeInternalError(c, "load trading rules", err)
		return
	}

	decision := kernel.Decision{
		Symbol:          req.Symbol,
		Action:          req.Action,
		Leverage:        req.Leverage,
		PositionSizeUSD: req.PositionSizeUSD,
		StopLoss:        req.StopLoss,
		TakeProfit:      req.TakeProfit,
		Confidence:      req.Confidence,
		Price:           req.Price,
	}
	// Price for percentage conditions: use request price, else fall back to live mark price
	price := req.Price
	if price <= 0 {
		price = getSymbolReferencePrice(req.Symbol)
	}

	violations, warnings := kernel.CheckDecisionAgainstRules(rules, decision, 0, price)
	lessons := kernel.MatchSoftRules(rules, decision)

	c.JSON(http.StatusOK, gin.H{
		"violations": violations,
		"warnings":   warnings,
		"lessons":    lessons,
		"blocked":    len(violations) > 0,
	})
}

// getSymbolReferencePrice fetches current price for percentage-based rule checks (best effort)
func getSymbolReferencePrice(symbol string) float64 {
	data, err := market.Get(symbol)
	if err != nil || data == nil {
		return 0
	}
	return data.CurrentPrice
}

// handleRuleExport GET /api/review/rules/export — JSON config export
func (s *Server) handleRuleExport(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	data, err := s.store.Rule().ExportRules(traderID)
	if err != nil {
		SafeInternalError(c, "export trading rules", err)
		return
	}
	c.Header("Content-Disposition", "attachment; filename=trading_rules.json")
	c.Data(http.StatusOK, "application/json", []byte(data))
}

// handleAIExtractRules POST /api/review/ai/extract-rules
// AI analyzes reviewed journal entries and proposes new rules
func (s *Server) handleAIExtractRules(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}

	// Gather data for AI: reviewed entries + existing stats
	entries, err := s.store.TradeJournal().GetRecentReviewed(traderID, 50)
	if err != nil || len(entries) == 0 {
		SafeBadRequest(c, "No reviewed journal entries yet. Review some trades first.")
		return
	}
	stats, err := s.store.TradeJournal().GetStats(traderID)
	if err != nil {
		SafeInternalError(c, "journal statistics", err)
		return
	}
	existingRules, _ := s.store.Rule().ListRules(traderID)

	// Build prompts
	systemPrompt, userPrompt := s.buildRuleExtractionPrompts(userID, entries, stats, existingRules)

	// Call AI using user's default enabled model
	aiResponse, err := s.callReviewAI(userID, systemPrompt, userPrompt)
	if err != nil {
		SafeError(c, http.StatusServiceUnavailable, "AI call failed: "+err.Error(), err)
		return
	}

	// Parse proposed rules
	proposals, err := parseRuleProposals(aiResponse)
	if err != nil {
		SafeError(c, http.StatusInternalServerError, "Failed to parse AI response: "+err.Error(), err)
		return
	}

	// review 2026-10-09 K: code-verify each proposal against ALL closed AI trades
	// (90d), not the model's free-text claim or the reviewed-only subset.
	out := make([]proposalView, 0, len(proposals))
	for _, p := range proposals {
		v, verr := s.verifyRule(userID, traderID, p.RuleType, p.Condition)
		if verr != nil {
			v = kernel.RuleVerification{Status: kernel.VerifyUnverifiable, Reason: "verification failed"}
		}
		out = append(out, proposalView{RuleInput: p, Verification: v})
	}

	c.JSON(http.StatusOK, gin.H{
		"proposals":   out,
		"ai_response": aiResponse,
	})
}

// proposalView an extracted proposal plus its server-side verification.
type proposalView struct {
	store.RuleInput
	Verification kernel.RuleVerification `json:"verification"`
}

// handleAIApplyRules POST /api/review/ai/apply-rules — save approved proposals
func (s *Server) handleAIApplyRules(c *gin.Context) {
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}
	var req struct {
		Rules []store.RuleInput `json:"rules"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		SafeBadRequest(c, "Invalid request body")
		return
	}
	if len(req.Rules) == 0 {
		SafeBadRequest(c, "No rules to apply")
		return
	}

	saved := 0
	rejected := []gin.H{}
	for i := range req.Rules {
		input := req.Rules[i]
		if err := validateRuleInput(&input, true); err != nil {
			rejected = append(rejected, gin.H{"index": i, "reason": err.Error()})
			continue
		}
		// Apply always means ai_review; recompute verification server-side
		// (never trust the client) — review 2026-10-09 K.
		input.Source = "ai_review"
		input.VerifiedStats, input.VerifiedAt = "", 0
		if input.RuleType == "hard" {
			v, verr := s.verifyRule(c.GetString("user_id"), traderID, input.RuleType, input.Condition)
			if verr != nil {
				rejected = append(rejected, gin.H{"index": i, "reason": "verification failed"})
				continue
			}
			if v.Status == kernel.VerifyWeak || v.Status == kernel.VerifyContradicted {
				rejected = append(rejected, gin.H{"index": i, "status": v.Status, "verification": v,
					"reason": fmt.Sprintf("rule not supported by history: %s (matched %d of %d closed AI trades in %dd, net %.2f)",
						v.Status, v.Matched, v.Population, kernel.RuleVerifyWindowDays, v.NetPnL)})
				continue
			}
			data, _ := json.Marshal(v)
			input.VerifiedStats, input.VerifiedAt = string(data), time.Now().UTC().UnixMilli()
		}
		if _, err := s.store.Rule().CreateRule(traderID, &input); err != nil {
			rejected = append(rejected, gin.H{"index": i, "reason": "rule persistence failed"})
			continue
		}
		saved++
	}
	c.JSON(http.StatusOK, gin.H{"saved": saved, "rejected": rejected})
}

// handleAIReview POST /api/review/ai/review
// AI performs a comprehensive review of the journal: strategy/execution/statistics layers
func (s *Server) handleAIReview(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID, ok := s.getJournalTrader(c)
	if !ok {
		return
	}

	var req struct {
		Period string `json:"period"` // daily|weekly|monthly
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		req.Period = "weekly"
	}

	// Gather recent journal entries (all statuses — pending ones still have trade facts)
	entries, _, err := s.store.TradeJournal().List(traderID, 100, 0, "", "")
	if err != nil || len(entries) == 0 {
		SafeBadRequest(c, "No trade journal entries yet.")
		return
	}
	stats, err := s.store.TradeJournal().GetStats(traderID)
	if err != nil {
		SafeInternalError(c, "journal statistics", err)
		return
	}
	// Include position-layer stats too
	posStats, _ := s.store.Position().GetFullStats(traderID, 0) // unknown base → 100 USDT fallback

	systemPrompt, userPrompt := s.buildReviewPrompts(userID, req.Period, entries, stats, posStats)

	aiResponse, err := s.callReviewAI(userID, systemPrompt, userPrompt)
	if err != nil {
		SafeError(c, http.StatusServiceUnavailable, "AI call failed: "+err.Error(), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ai_response":  aiResponse,
		"period":       req.Period,
		"generated_at": time.Now().UTC().UnixMilli(),
	})
}

// callReviewAI invokes the user's default enabled AI model
func (s *Server) callReviewAI(userID, systemPrompt, userPrompt string) (string, error) {
	model, err := s.store.AIModel().GetDefault(userID)
	if err != nil || model == nil {
		model, err = s.store.AIModel().GetAnyEnabled()
		if err != nil || model == nil {
			return "", fmt.Errorf("no enabled AI model found, please configure one in settings")
		}
	}
	if model.APIKey == "" {
		return "", fmt.Errorf("AI model %s is missing API Key", model.Name)
	}

	aiClient := mcp.NewAIClientByProvider(model.Provider)
	if aiClient == nil {
		aiClient = mcp.NewClient()
	}
	aiClient.SetAPIKey(string(model.APIKey), model.CustomAPIURL, model.CustomModelName)

	return aiClient.CallWithMessages(systemPrompt, userPrompt)
}

// Default review prompt templates. Used when the user has no custom prompt
// configured (GET /api/review/prompt-config returns these as the baseline).
const (
	defaultReviewSystemPrompt = `You are a professional trading performance coach conducting a structured trade review (复盘).

Analyze the trade journal and respond in the trader's language with these three layers, in this exact structure:

## 策略层面（策略本身是否有问题）
- Which entry logics actually worked vs failed (was it luck or logic)?
- Risk/reward assessment: is the current SL/TP ratio sustainable?
- Which strategy types are truly effective? Break down by strategy_tag.

## 执行层面（是否做到知行合一）
- Plan adherence: how often did trades follow the planned SL/TP?
- Position sizing consistency.
- Emotional patterns (fomo/revenge/fear of missing) and their cost.

## 量化指标解读
- Win rate, profit/loss ratio, expectancy, max drawdown — interpret each and state whether the strategy is worth continuing.

## 下月具体行动规则
- Propose 2-3 CONCRETE rules (specific numbers, not vague advice like "be more cautious").
- Format each as: [RULE] rule name | hard:condition 或 soft:lesson text

Be specific, cite actual trades (symbol + PnL) as evidence. Do not invent data.`

	defaultRuleExtractSystemPrompt = `You are a professional trading performance coach. Your job is to extract explicit, actionable trading rules from a trader's reviewed trade journal.

Return ONLY a JSON array (no markdown fences, no explanation) of proposed rules:
[
  {
    "rule_type": "hard" | "soft",
    "name": "short rule name in the trader's language",
    "description": "hard rule description in the trader's language",
    "condition": {"field": "leverage|position_size_usd|position_value_pct|stop_loss_pct|take_profit_pct|risk_reward|confidence|has_stop_loss|has_take_profit|symbol", "op": ">|>=|<|<=|==|!=|in", "value": <number|string|bool>},
    "on_violation": "block" | "warn",
    "lesson_text": "soft rule lesson text in the trader's language",
    "tags": "comma separated tags",
    "source_stats": "supporting data, e.g. win_rate=15%,sample=8",
    "supporting_trades": <int, number of journal trades that support this rule>
  }
]

Guidelines:
- hard rules: machine-checkable constraints (condition must be a single evaluable condition), used when the data shows a clear, repeated pattern of losses.
- soft rules: text lessons (lesson_text), used for behavioral/emotional patterns that cannot be expressed as a condition.
- Only propose rules strongly supported by the data (at least 3 supporting trades).
- Do NOT duplicate existing rules.
- Propose at most 5 rules. If the data supports no new rule, return [].`
)

// userReviewPromptConfig loads the user's custom prompt templates (nil when
// none customized — callers then fall back to the defaults above).
func (s *Server) userReviewPromptConfig(userID string) *store.ReviewPromptConfig {
	cfg, err := s.store.ReviewPrompt().Get(userID)
	if err != nil {
		return nil
	}
	return cfg
}

// handleGetReviewPromptConfig GET /api/review/prompt-config
// Returns the user's custom prompt templates plus the built-in defaults so
// the UI can show what's in effect and offer "restore default".
func (s *Server) handleGetReviewPromptConfig(c *gin.Context) {
	userID := c.GetString("user_id")
	cfg, err := s.store.ReviewPrompt().Get(userID)
	if err != nil {
		SafeInternalError(c, "load review prompt config", err)
		return
	}
	resp := gin.H{
		"review_system_prompt":       defaultReviewSystemPrompt,
		"rule_extract_system_prompt": defaultRuleExtractSystemPrompt,
		"review_custom":              false,
		"rule_extract_custom":        false,
	}
	if cfg != nil {
		if cfg.ReviewSystemPrompt != "" {
			resp["review_system_prompt"] = cfg.ReviewSystemPrompt
			resp["review_custom"] = true
		}
		if cfg.RuleExtractSystemPrompt != "" {
			resp["rule_extract_system_prompt"] = cfg.RuleExtractSystemPrompt
			resp["rule_extract_custom"] = true
		}
	}
	c.JSON(http.StatusOK, resp)
}

// handleUpdateReviewPromptConfig PUT /api/review/prompt-config
// Saves custom prompt templates. Empty string = reset that template to the
// built-in default.
func (s *Server) handleUpdateReviewPromptConfig(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		ReviewSystemPrompt      *string `json:"review_system_prompt"`
		RuleExtractSystemPrompt *string `json:"rule_extract_system_prompt"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		SafeBadRequest(c, "Invalid request parameters")
		return
	}
	if req.ReviewSystemPrompt == nil && req.RuleExtractSystemPrompt == nil {
		SafeBadRequest(c, "nothing to update")
		return
	}

	// Start from the current config so a partial update preserves the other
	// template.
	reviewPrompt := ""
	ruleExtractPrompt := ""
	if cfg, _ := s.store.ReviewPrompt().Get(userID); cfg != nil {
		reviewPrompt = cfg.ReviewSystemPrompt
		ruleExtractPrompt = cfg.RuleExtractSystemPrompt
	}
	if req.ReviewSystemPrompt != nil {
		reviewPrompt = strings.TrimSpace(*req.ReviewSystemPrompt)
	}
	if req.RuleExtractSystemPrompt != nil {
		ruleExtractPrompt = strings.TrimSpace(*req.RuleExtractSystemPrompt)
	}

	if err := s.store.ReviewPrompt().Upsert(userID, reviewPrompt, ruleExtractPrompt); err != nil {
		SafeInternalError(c, "save review prompt config", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "saved"})
}

func (s *Server) buildRuleExtractionPrompts(userID string, entries []*store.TradeJournalDB, stats *store.JournalStats, existingRules []*store.TradingRuleDB) (string, string) {
	systemPrompt := defaultRuleExtractSystemPrompt
	if cfg := s.userReviewPromptConfig(userID); cfg != nil && cfg.RuleExtractSystemPrompt != "" {
		systemPrompt = cfg.RuleExtractSystemPrompt
	}

	// Compact journal dump
	var sb strings.Builder
	sb.WriteString("## Reviewed Trade Journal\n")
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("- %s %s | entry %.4f exit %.4f | %dx | PnL(净) %+.2f (%+.1f%%) | plan:SL=%.4f,TP=%.4f | adherence=%s | emotions=%s | category=%s | strategy=%s | lesson=%s\n",
			e.Symbol, e.Side, e.EntryPrice, e.ExitPrice, e.Leverage, e.RealizedPnL-e.Fee, e.PnLPct,
			e.PlannedStopLoss, e.PlannedTakeProfit, orDash(e.ExecutedAsPlan), orDash(e.Emotions),
			orDash(e.MistakeCategory), orDash(e.StrategyTag), orDash(e.Lesson)))
	}

	data, _ := json.Marshal(stats)
	sb.WriteString(fmt.Sprintf("\n## Aggregate Statistics\n%s\n", string(data)))

	if len(existingRules) > 0 {
		sb.WriteString("## Existing Rules (do not duplicate)\n")
		for _, r := range existingRules {
			if r.RuleType == "hard" {
				sb.WriteString(fmt.Sprintf("- [hard] %s %s\n", r.Name, r.ConditionJSON))
			} else {
				sb.WriteString(fmt.Sprintf("- [soft] %s: %s\n", r.Name, r.LessonText))
			}
		}
	}

	return systemPrompt, sb.String()
}

// buildReviewPrompts builds prompts for comprehensive AI review
func (s *Server) buildReviewPrompts(userID, period string, entries []*store.TradeJournalDB, stats *store.JournalStats, posStats *store.TraderStats) (string, string) {
	systemPrompt := defaultReviewSystemPrompt
	if cfg := s.userReviewPromptConfig(userID); cfg != nil && cfg.ReviewSystemPrompt != "" {
		systemPrompt = cfg.ReviewSystemPrompt
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Review Period Request: %s\n\n", period))
	sb.WriteString("## Trade Journal (most recent 100)\n")
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("- %s %s | entry %.4f exit %.4f | %dx | PnL(净) %+.2f (%+.1f%%) | plan:SL=%.4f,TP=%.4f | adherence=%s | emotions=%s | category=%s | strategy=%s | lesson=%s\n",
			e.Symbol, e.Side, e.EntryPrice, e.ExitPrice, e.Leverage, e.RealizedPnL-e.Fee, e.PnLPct,
			e.PlannedStopLoss, e.PlannedTakeProfit, orDash(e.ExecutedAsPlan), orDash(e.Emotions),
			orDash(e.MistakeCategory), orDash(e.StrategyTag), orDash(e.Lesson)))
	}

	data, _ := json.Marshal(stats)
	sb.WriteString(fmt.Sprintf("\n## Review-layer Statistics\n%s\n", string(data)))

	if posStats != nil && posStats.TotalTrades > 0 {
		sb.WriteString(fmt.Sprintf("## Account-level Statistics\ntotal_trades=%d win_rate=%.1f%% profit_factor=%.2f sharpe=%.2f avg_win=%.2f avg_loss=%.2f max_drawdown=%.1f%%\n",
			posStats.TotalTrades, posStats.WinRate, posStats.ProfitFactor, posStats.SharpeRatio,
			posStats.AvgWin, posStats.AvgLoss, posStats.MaxDrawdownPct))
	}

	return systemPrompt, sb.String()
}

// parseRuleProposals parses AI-proposed rules from the response
func parseRuleProposals(response string) ([]store.RuleInput, error) {
	text := strings.TrimSpace(response)
	// Strip markdown code fences if present
	if idx := strings.Index(text, "["); idx >= 0 {
		if end := strings.LastIndex(text, "]"); end > idx {
			text = text[idx : end+1]
		}
	}

	var raw []struct {
		RuleType    string      `json:"rule_type"`
		Name        string      `json:"name"`
		Description string      `json:"description"`
		Condition   interface{} `json:"condition"`
		OnViolation string      `json:"on_violation"`
		LessonText  string      `json:"lesson_text"`
		Tags        string      `json:"tags"`
		SourceStats string      `json:"source_stats"`
		Supporting  int         `json:"supporting_trades"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("AI response is not a valid rule array: %w", err)
	}

	proposals := make([]store.RuleInput, 0, len(raw))
	for _, r := range raw {
		condJSON := ""
		if r.Condition != nil {
			data, err := json.Marshal(r.Condition)
			if err == nil {
				condJSON = string(data)
			}
		}
		if r.OnViolation == "" {
			r.OnViolation = "warn"
		}
		proposals = append(proposals, store.RuleInput{
			RuleType:    r.RuleType,
			Name:        r.Name,
			Description: r.Description,
			Condition:   condJSON,
			OnViolation: r.OnViolation,
			LessonText:  r.LessonText,
			Tags:        r.Tags,
			SourceStats: r.SourceStats,
			Enabled:     boolPtr(true),

			SupportingTrades: r.Supporting,
		})
	}
	return proposals, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func boolPtr(b bool) *bool { return &b }
