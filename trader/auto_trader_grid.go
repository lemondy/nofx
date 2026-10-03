package trader

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"sync"
	"time"
)

// ============================================================================
// Grid Trading State Management
// ============================================================================

// GridState holds the runtime state for grid trading
type GridState struct {
	mu sync.RWMutex

	// Configuration
	Config *store.GridStrategyConfig

	// Grid levels
	Levels []kernel.GridLevelInfo

	// Calculated bounds
	UpperPrice  float64
	LowerPrice  float64
	GridSpacing float64

	// State flags
	IsPaused      bool
	PauseReason   string
	IsInitialized bool

	// Performance tracking
	TotalProfit    float64
	TotalTrades    int
	WinningTrades  int
	MaxDrawdown    float64
	PeakEquity     float64
	DailyPnL       float64
	LastDailyReset time.Time
	// F14 (2026-10-01 review): the daily-loss halt measures LIVE equity
	// against the first equity seen this day — the ledger figure only ever
	// saw the internal stop-loss estimates (exchange closes, fees and
	// funding never updated it).
	DayStartDay    string
	DayStartEquity float64

	// Order tracking
	OrderBook map[string]int // OrderID -> LevelIndex

	// Box state
	ShortBoxUpper float64
	ShortBoxLower float64
	MidBoxUpper   float64
	MidBoxLower   float64
	LongBoxUpper  float64
	LongBoxLower  float64

	// Breakout state
	BreakoutLevel        string
	BreakoutDirection    string
	BreakoutConfirmCount int

	// Position reduction (0 = normal, 50 = reduced after false breakout)
	PositionReductionPct float64

	// Current regime level
	CurrentRegimeLevel string

	// Grid direction adjustment
	CurrentDirection     market.GridDirection
	DirectionChangedAt   time.Time
	DirectionChangeCount int
}

// NewGridState creates a new grid state
func NewGridState(config *store.GridStrategyConfig) *GridState {
	return &GridState{
		Config:           config,
		Levels:           make([]kernel.GridLevelInfo, 0),
		OrderBook:        make(map[string]int),
		CurrentDirection: market.GridDirectionNeutral,
	}
}

// ============================================================================
// Breakout Detection (price vs grid boundary)
// ============================================================================

// BreakoutType represents the type of price breakout
type BreakoutType string

const (
	BreakoutNone  BreakoutType = "none"
	BreakoutUpper BreakoutType = "upper"
	BreakoutLower BreakoutType = "lower"
)

// checkBreakout detects if price has broken out of grid range
// Returns breakout type and percentage beyond boundary
func (at *AutoTrader) checkBreakout() (BreakoutType, float64) {
	gridConfig := at.config.StrategyConfig.GridConfig

	currentPrice, err := at.trader.GetMarketPrice(gridConfig.Symbol)
	if err != nil {
		return BreakoutNone, 0
	}

	at.gridState.mu.RLock()
	upper := at.gridState.UpperPrice
	lower := at.gridState.LowerPrice
	at.gridState.mu.RUnlock()

	if upper <= 0 || lower <= 0 {
		return BreakoutNone, 0
	}

	// Check upper breakout
	if currentPrice > upper {
		breakoutPct := (currentPrice - upper) / upper * 100
		return BreakoutUpper, breakoutPct
	}

	// Check lower breakout
	if currentPrice < lower {
		breakoutPct := (lower - currentPrice) / lower * 100
		return BreakoutLower, breakoutPct
	}

	return BreakoutNone, 0
}

// gridAccountEquity reads the unified account equity from a balance map
// (F14, 2026-10-01 review). Adapters publish "totalEquity" (camelCase —
// binance/bybit/okx/hyperliquid); the old reads keyed on "total_equity"
// (snake_case) which NO adapter sets, so the primary branch never hit and
// OKX's UPL got double-counted through the wallet+uPnL fallback (OKX maps
// totalEq — which already includes UPL — into both keys). Order:
// totalEquity → total_equity (legacy) → wallet+uPnL.
func gridAccountEquity(balance map[string]interface{}) float64 {
	if equity, ok := balance["totalEquity"].(float64); ok {
		if equity > 0 && !math.IsNaN(equity) && !math.IsInf(equity, 0) {
			return equity
		}
		return 0
	}
	if equity, ok := balance["total_equity"].(float64); ok {
		if equity > 0 && !math.IsNaN(equity) && !math.IsInf(equity, 0) {
			return equity
		}
		return 0
	}
	total, hasTotal := balance["totalWalletBalance"].(float64)
	unrealized, hasUPL := balance["totalUnrealizedProfit"].(float64)
	if hasTotal && hasUPL && total+unrealized > 0 && !math.IsNaN(total+unrealized) && !math.IsInf(total+unrealized, 0) {
		return total + unrealized
	}
	return 0
}

// checkMaxDrawdown checks if current drawdown exceeds maximum allowed
// Returns: (exceeded bool, currentDrawdown float64)
func (at *AutoTrader) checkMaxDrawdown() (bool, float64) {
	gridConfig := at.config.StrategyConfig.GridConfig
	if gridConfig.MaxDrawdownPct <= 0 {
		return false, 0
	}

	// Get current equity. 2026-10-03 review P2: transient failures here used
	// to report "drawdown 100%" and trip the grid breaker on a data hiccup —
	// skip the check this round instead (fail-open with a log; the next cycle
	// retries and the exchange-side stops still protect positions).
	balance, err := at.trader.GetBalance()
	if err != nil {
		logger.Warnf("⚠️ [Grid] drawdown check skipped: balance fetch failed: %v", err)
		return false, 0
	}
	currentEquity := gridAccountEquity(balance)

	if currentEquity <= 0 {
		logger.Warnf("⚠️ [Grid] drawdown check skipped: equity unreadable/zero")
		return false, 0
	}

	at.gridState.mu.RLock()
	seedPeak := math.Max(currentEquity, at.gridState.PeakEquity)
	at.gridState.mu.RUnlock()
	peakEquity := seedPeak
	if at.store != nil {
		var persistErr error
		peakEquity, persistErr = at.store.RiskState().UpdatePeak(at.dailyBaselineKey(), seedPeak)
		if persistErr != nil {
			// Transient store error ≠ a real drawdown — skip this round.
			logger.Warnf("⚠️ [Grid] drawdown check skipped: peak persist failed: %v", persistErr)
			return false, 0
		}
	} else {
		dailyBaselineRegMu.Lock()
		key := "peak:" + at.dailyBaselineKey()
		rec := dailyBaselineReg[key]
		if seedPeak > rec.equity {
			rec.equity = seedPeak
			dailyBaselineReg[key] = rec
		}
		peakEquity = rec.equity
		dailyBaselineRegMu.Unlock()
	}
	at.gridState.mu.Lock()
	at.gridState.PeakEquity = peakEquity
	at.gridState.mu.Unlock()

	if peakEquity <= 0 {
		return false, 0
	}

	// Calculate current drawdown
	drawdown := (peakEquity - currentEquity) / peakEquity * 100

	// Update max drawdown tracking
	at.gridState.mu.Lock()
	if drawdown > at.gridState.MaxDrawdown {
		at.gridState.MaxDrawdown = drawdown
	}
	at.gridState.mu.Unlock()

	return drawdown >= gridConfig.MaxDrawdownPct, drawdown
}

// checkDailyLossLimit checks if daily loss exceeds limit
// Returns: (exceeded bool, dailyLossPct float64)
//
// F14 (2026-10-01 review): the ledger figure (DailyPnL) only ever saw the
// internal stop-loss ESTIMATES — updateDailyPnL has no callers, so
// exchange-executed closes, fees and funding never counted. The halt now
// measures LIVE equity against the day-anchored baseline (same contract as
// the main trader's halt) and takes the WORSE of the two measures, so the
// internal estimates can only ever trip the halt earlier.
func (at *AutoTrader) checkDailyLossLimit() (bool, float64) {
	gridConfig := at.config.StrategyConfig.GridConfig
	if gridConfig.DailyLossLimitPct <= 0 {
		return false, 0
	}

	equityLossPct, equityKnown := at.gridEquityDailyLossPct()

	at.gridState.mu.Lock()
	// Reset daily PnL if new day
	now := time.Now()
	if now.YearDay() != at.gridState.LastDailyReset.YearDay() ||
		now.Year() != at.gridState.LastDailyReset.Year() {
		at.gridState.DailyPnL = 0
		at.gridState.LastDailyReset = now
	}
	dailyPnL := at.gridState.DailyPnL
	at.gridState.mu.Unlock()

	ledgerPct := 0.0
	if gridConfig.TotalInvestment > 0 && dailyPnL < 0 {
		ledgerPct = (-dailyPnL) / gridConfig.TotalInvestment * 100
	}
	if !equityKnown {
		return true, ledgerPct // cannot admit risk without a verified equity baseline
	}
	dailyLossPct := equityLossPct
	if ledgerPct > dailyLossPct {
		dailyLossPct = ledgerPct
	}
	return dailyLossPct >= gridConfig.DailyLossLimitPct, dailyLossPct
}

// gridEquityDailyLossPct anchors once per day at the first readable equity
// and measures the current loss from that baseline.
func (at *AutoTrader) gridEquityDailyLossPct() (float64, bool) {
	balance, err := at.trader.GetBalance()
	if err != nil {
		return 0, false
	}
	equity := gridAccountEquity(balance)
	if equity <= 0 {
		return 0, false
	}
	today := time.Now().UTC().Format("2006-01-02")
	var baseline float64
	if at.store != nil {
		baseline, err = at.store.RiskState().AnchorDayBaseline(at.dailyBaselineKey(), today, equity)
		if err != nil {
			return 0, false
		}
	} else {
		at.anchorDailyBaseline(equity)
		baseline = at.dayStartEquity
	}
	at.gridState.mu.Lock()
	at.gridState.DayStartDay = today
	at.gridState.DayStartEquity = baseline
	at.gridState.mu.Unlock()
	return math.Max(0, (baseline-equity)/baseline*100), true
}

// updateDailyPnL updates the daily PnL tracking
func (at *AutoTrader) updateDailyPnL(realizedPnL float64) {
	at.gridState.mu.Lock()
	at.gridState.DailyPnL += realizedPnL
	at.gridState.TotalProfit += realizedPnL
	at.gridState.mu.Unlock()
}

// emergencyExit closes all positions and cancels all orders
func (at *AutoTrader) emergencyExit(reason string) error {
	cancelErr := at.pauseGrid(reason)
	closeErr := at.closeAllPositions()
	return errors.Join(cancelErr, closeErr)
}

// handleBreakout handles price breakout from grid range
func (at *AutoTrader) handleBreakout(breakoutType BreakoutType, breakoutPct float64) error {
	logger.Warnf("[Grid] BREAKOUT DETECTED: %s, %.2f%% beyond boundary", breakoutType, breakoutPct)

	// If breakout exceeds 2%, pause grid and cancel orders
	if breakoutPct >= 2.0 {
		logger.Warnf("[Grid] Significant breakout (%.2f%%), pausing grid and canceling orders", breakoutPct)

		return errors.Join(fmt.Errorf("grid paused due to %s breakout (%.2f%%)", breakoutType, breakoutPct), at.pauseGrid("breakout"))
	}

	// If breakout is minor (< 2%), consider adjusting grid
	if breakoutPct >= 1.0 {
		logger.Infof("[Grid] Minor breakout (%.2f%%), considering grid adjustment", breakoutPct)
		// Let AI decide whether to adjust
	}

	return nil
}

// ============================================================================
// AutoTrader Grid Lifecycle
// ============================================================================

// InitializeGrid initializes the grid state and calculates levels
func (at *AutoTrader) InitializeGrid() error {
	if at.config.StrategyConfig == nil || at.config.StrategyConfig.GridConfig == nil {
		return fmt.Errorf("grid configuration not found")
	}

	gridConfig := at.config.StrategyConfig.GridConfig
	if err := gridConfig.Validate(); err != nil {
		return err
	}
	at.runtimeMu.Lock()
	defer at.runtimeMu.Unlock()
	at.gridState = NewGridState(gridConfig)
	if at.store != nil {
		row, err := at.store.PendingEntry().LoadGrid(at.id)
		if err != nil {
			return fmt.Errorf("grid recovery ledger unavailable: %w", err)
		}
		if row != nil {
			restored := NewGridState(gridConfig)
			if err := json.Unmarshal([]byte(row.StateJSON), restored); err != nil {
				return fmt.Errorf("grid recovery ledger corrupt: %w", err)
			}
			active := false
			for _, level := range restored.Levels {
				if level.PositionSize > 0 || level.OrderID != "" || level.ExitOrderID != "" {
					active = true
				}
			}
			if active {
				if row.Symbol != gridConfig.Symbol || row.ExchangeID != at.executionAccountKey() {
					return fmt.Errorf("grid account/symbol cannot change with unresolved orders or positions")
				}
				restored.Config = gridConfig
				restored.OrderBook = map[string]int{}
				for i, level := range restored.Levels {
					if level.OrderID != "" {
						restored.OrderBook[level.OrderID] = i
					}
				}
				at.gridState = restored
				// Recover pending receipt tasks before deciding whether to allow entries.
				at.syncGridState()
				// A successful user stop may resume on explicit restart. Risk or
				// uncertain cancellation pauses remain in force across restart.
				if restored.PauseReason == "trader stopped" {
					unresolved := false
					for _, level := range restored.Levels {
						unresolved = unresolved || level.OrderID != "" || level.ExitOrderID != ""
					}
					if !unresolved {
						restored.IsPaused = false
						restored.PauseReason = ""
						return at.persistGridLedger()
					}
				}
				return nil
			}
		}
	}

	// Get current market price
	price, err := at.trader.GetMarketPrice(gridConfig.Symbol)
	if err != nil {
		return fmt.Errorf("failed to get market price: %w", err)
	}

	// Calculate grid bounds
	if gridConfig.UseATRBounds {
		// Get ATR for bound calculation
		mktData, err := at.getMarketTimeframes(gridConfig.Symbol, []string{"4h"}, "4h", 20)
		if err != nil {
			logger.Warnf("Failed to get market data for ATR: %v, using default bounds", err)
			at.calculateDefaultBounds(price, gridConfig)
		} else {
			at.calculateATRBounds(price, mktData, gridConfig)
		}
	} else {
		// Use manual bounds
		at.gridState.UpperPrice = gridConfig.UpperPrice
		at.gridState.LowerPrice = gridConfig.LowerPrice
	}

	// Calculate grid spacing
	if at.gridState.LowerPrice <= 0 || at.gridState.UpperPrice <= at.gridState.LowerPrice {
		return fmt.Errorf("invalid calculated grid bounds")
	}
	at.gridState.GridSpacing = (at.gridState.UpperPrice - at.gridState.LowerPrice) / float64(gridConfig.GridCount-1)

	// Initialize grid levels
	at.initializeGridLevels(price, gridConfig)

	// CRITICAL: Set leverage on exchange before trading
	if err := at.trader.SetLeverage(gridConfig.Symbol, gridConfig.Leverage); err != nil {
		return fmt.Errorf("failed to set grid leverage: %w", err)
	} else {
		logger.Infof("[Grid] Leverage set to %dx for %s", gridConfig.Leverage, gridConfig.Symbol)
	}

	at.gridState.IsInitialized = true
	logger.Infof("[Grid] Initialized: %d levels, $%.2f - $%.2f, spacing $%.2f",
		gridConfig.GridCount, at.gridState.LowerPrice, at.gridState.UpperPrice, at.gridState.GridSpacing)

	return at.persistGridLedger()
}

// RunGridCycle executes one grid trading cycle
func (at *AutoTrader) RunGridCycle() error {
	mu := at.executionMutex()
	mu.Lock()
	defer mu.Unlock()
	// Check if trader is stopped (early exit to prevent trades after Stop() is called)
	at.isRunningMutex.RLock()
	running := at.isRunning
	at.isRunningMutex.RUnlock()
	if !running {
		logger.Infof("[Grid] Trader is stopped, aborting grid cycle")
		return nil
	}

	if at.gridState == nil || !at.gridState.IsInitialized {
		if err := at.InitializeGrid(); err != nil {
			return fmt.Errorf("failed to initialize grid: %w", err)
		}
	}

	at.syncGridState()
	at.checkAndExecuteStopLoss()
	// CRITICAL: Check for breakout before executing any trades
	breakoutType, breakoutPct := at.checkBreakout()
	if breakoutType != BreakoutNone {
		if err := at.handleBreakout(breakoutType, breakoutPct); err != nil {
			return err // Grid paused due to breakout
		}
	}

	// CRITICAL: Check max drawdown
	exceeded, drawdown := at.checkMaxDrawdown()
	if exceeded {
		return at.emergencyExit(fmt.Sprintf("max drawdown exceeded: %.2f%%", drawdown))
	}

	// CRITICAL: Check daily loss limit
	dailyExceeded, dailyLossPct := at.checkDailyLossLimit()
	if dailyExceeded {
		logger.Errorf("[Grid] Daily loss limit exceeded: %.2f%%", dailyLossPct)
		return errors.Join(fmt.Errorf("daily loss limit exceeded: %.2f%%", dailyLossPct), at.pauseGrid("daily loss limit"))
	}

	// Check multi-period box breakout
	if err := at.checkBoxBreakout(); err != nil {
		logger.Infof("Box breakout check error: %v", err)
	}

	// Check for false breakout recovery
	if err := at.checkFalseBreakoutRecovery(); err != nil {
		logger.Infof("False breakout recovery check error: %v", err)
	}

	// Check if grid is paused
	at.gridState.mu.RLock()
	isPaused := at.gridState.IsPaused
	at.gridState.mu.RUnlock()
	if isPaused {
		logger.Infof("[Grid] Grid is paused, skipping cycle")
		return nil
	}

	at.autoAdjustGrid()
	gridConfig := at.config.StrategyConfig.GridConfig
	lang := at.config.StrategyConfig.Language
	if lang == "" {
		lang = "en"
	}

	// Build grid context
	gridCtx, err := at.buildGridContext()
	if err != nil {
		return fmt.Errorf("failed to build grid context: %w", err)
	}

	// Get AI decisions
	decision, err := withoutExecutionLock(mu, func() (*kernel.FullDecision, error) {
		return kernel.GetGridDecisions(gridCtx, at.mcpClient, gridConfig, lang)
	})
	if err != nil {
		return fmt.Errorf("failed to get grid decisions: %w", err)
	}

	// Check if trader is stopped before executing any decisions (prevent trades after Stop())
	at.isRunningMutex.RLock()
	running = at.isRunning
	at.isRunningMutex.RUnlock()
	if !running {
		logger.Infof("[Grid] Trader stopped before decision execution, aborting grid cycle")
		return nil
	}

	// AI latency may span a loss-halt transition or another trader's execution.
	at.syncGridState()
	if exceeded, drawdown := at.checkMaxDrawdown(); exceeded {
		return at.emergencyExit(fmt.Sprintf("max drawdown exceeded: %.2f%%", drawdown))
	}
	if exceeded, loss := at.checkDailyLossLimit(); exceeded {
		return errors.Join(fmt.Errorf("daily loss limit exceeded: %.2f%%", loss), at.pauseGrid("daily loss limit"))
	}
	at.gridState.mu.RLock()
	paused := at.gridState.IsPaused
	at.gridState.mu.RUnlock()
	if paused {
		return nil
	}

	// Execute decisions
	executionFailures := map[string]string{}
	for _, d := range decision.Decisions {
		// Check if trader is still running before each decision
		at.isRunningMutex.RLock()
		running := at.isRunning
		at.isRunningMutex.RUnlock()
		if !running {
			logger.Infof("[Grid] Trader stopped, skipping remaining %d decisions", len(decision.Decisions))
			break
		}
		if err := at.executeGridDecision(&d); err != nil {
			logger.Warnf("[Grid] Failed to execute decision %s: %v", d.Action, err)
			// F12 (2026-10-01 review): a failed placement must be visible in
			// the decision record — everything used to land as Success:true.
			executionFailures[d.Action+"|"+d.Symbol] = err.Error()
		}
	}

	// Sync state with exchange
	at.syncGridState()

	// Save decision record
	at.saveGridDecisionRecordWithFailures(decision, executionFailures)

	return nil
}

// buildGridContext builds the context for AI grid decisions
func (at *AutoTrader) buildGridContext() (*kernel.GridContext, error) {
	gridConfig := at.config.StrategyConfig.GridConfig

	// Get market data
	mktData, err := at.getMarketTimeframes(gridConfig.Symbol, []string{"5m", "4h"}, "5m", 50)
	if err != nil {
		return nil, fmt.Errorf("failed to get market data: %w", err)
	}

	// Build base context from market data
	ctx := kernel.BuildGridContextFromMarketData(mktData, gridConfig)

	// Add grid state
	at.gridState.mu.RLock()
	ctx.Levels = at.gridState.Levels
	ctx.UpperPrice = at.gridState.UpperPrice
	ctx.LowerPrice = at.gridState.LowerPrice
	ctx.GridSpacing = at.gridState.GridSpacing
	ctx.IsPaused = at.gridState.IsPaused
	ctx.TotalProfit = at.gridState.TotalProfit
	ctx.TotalTrades = at.gridState.TotalTrades
	ctx.WinningTrades = at.gridState.WinningTrades
	ctx.MaxDrawdown = at.gridState.MaxDrawdown
	ctx.DailyPnL = at.gridState.DailyPnL

	// Count active orders and filled levels
	for _, level := range at.gridState.Levels {
		if level.State == "pending" {
			ctx.ActiveOrderCount++
		} else if level.State == "filled" {
			ctx.FilledLevelCount++
		}
	}
	at.gridState.mu.RUnlock()

	// Get account info
	balance, err := at.trader.GetBalance()
	if err == nil {
		// F14 (2026-10-01 review): adapters publish "totalEquity" — the old
		// snake_case key never matched and grid context equity was 0.
		if equity := gridAccountEquity(balance); equity > 0 {
			ctx.TotalEquity = equity
		}
		if available, ok := balance["availableBalance"].(float64); ok {
			ctx.AvailableBalance = available
		}
		if unrealized, ok := balance["totalUnrealizedProfit"].(float64); ok {
			ctx.UnrealizedPnL = unrealized
		}
	}

	// Get current position
	positions, err := at.trader.GetPositions()
	if err == nil {
		for _, pos := range positions {
			if sym, ok := pos["symbol"].(string); ok && sym == gridConfig.Symbol {
				if size, ok := pos["positionAmt"].(float64); ok {
					ctx.CurrentPosition = size
				}
			}
		}
	}

	return ctx, nil
}

// executeGridDecision executes a single grid decision
func (at *AutoTrader) executeGridDecision(d *kernel.Decision) error {
	switch d.Action {
	case "place_buy_limit":
		return at.placeGridLimitOrder(d, "BUY")
	case "place_sell_limit":
		return at.placeGridLimitOrder(d, "SELL")
	case "cancel_order":
		return at.cancelGridOrder(d)
	case "cancel_all_orders":
		return at.cancelAllGridOrders()
	case "pause_grid":
		return at.pauseGrid(d.Reasoning)
	case "resume_grid":
		return at.resumeGrid()
	case "adjust_grid":
		return at.adjustGrid(d)
	case "hold":
		logger.Infof("[Grid] Holding current state: %s", d.Reasoning)
		return nil
	// Support standard actions for closing positions
	case "close_long":
		_, err := at.trader.CloseLong(d.Symbol, d.Quantity)
		return err
	case "close_short":
		_, err := at.trader.CloseShort(d.Symbol, d.Quantity)
		return err
	default:
		logger.Warnf("[Grid] Unknown action: %s", d.Action)
		return nil
	}
}

// IsGridStrategy returns true if current strategy is grid trading
func (at *AutoTrader) IsGridStrategy() bool {
	if at.config.StrategyConfig == nil {
		return false
	}
	return at.config.StrategyConfig.StrategyType == "grid_trading" && at.config.StrategyConfig.GridConfig != nil
}

// saveGridDecisionRecord saves the grid decision to database
func (at *AutoTrader) saveGridDecisionRecord(decision *kernel.FullDecision) {
	at.saveGridDecisionRecordWithFailures(decision, nil)
}

func (at *AutoTrader) saveGridDecisionRecordWithFailures(decision *kernel.FullDecision, executionFailures map[string]string) {
	if at.store == nil {
		return
	}

	at.cycleNumber++

	record := &store.DecisionRecord{
		TraderID:            at.id,
		CycleNumber:         at.cycleNumber,
		Timestamp:           time.Now().UTC(),
		SystemPrompt:        decision.SystemPrompt,
		InputPrompt:         decision.UserPrompt,
		CoTTrace:            decision.CoTTrace,
		RawResponse:         decision.RawResponse,
		AIRequestDurationMs: decision.AIRequestDurationMs,
		Success:             len(executionFailures) == 0,
	}

	if len(decision.Decisions) > 0 {
		decisionJSON, _ := json.MarshalIndent(decision.Decisions, "", "  ")
		record.DecisionJSON = string(decisionJSON)

		// Convert kernel.Decision to store.DecisionAction for frontend display
		for _, d := range decision.Decisions {
			success := true
			reasoning := d.Reasoning
			if failure, bad := executionFailures[d.Action+"|"+d.Symbol]; bad {
				success = false
				reasoning = fmt.Sprintf("%s\n[EXECUTION FAILED] %s", reasoning, failure)
			}
			actionRecord := store.DecisionAction{
				Action:     d.Action,
				Symbol:     d.Symbol,
				Quantity:   d.Quantity,
				Leverage:   d.Leverage,
				Price:      d.Price,
				StopLoss:   d.StopLoss,
				TakeProfit: d.TakeProfit,
				Confidence: d.Confidence,
				Reasoning:  reasoning,
				Timestamp:  time.Now().UTC(),
				Success:    success,
			}
			record.Decisions = append(record.Decisions, actionRecord)
		}
	}

	record.ExecutionLog = append(record.ExecutionLog, fmt.Sprintf("Grid cycle completed with %d decisions (%d failed)", len(decision.Decisions), len(executionFailures)))

	if err := at.store.Decision().LogDecision(record); err != nil {
		logger.Warnf("[Grid] Failed to save decision record: %v", err)
	}
}

// GridRiskInfo contains risk information for frontend display
type GridRiskInfo struct {
	CurrentLeverage     int     `json:"current_leverage"`
	EffectiveLeverage   float64 `json:"effective_leverage"`
	RecommendedLeverage int     `json:"recommended_leverage"`

	CurrentPosition float64 `json:"current_position"`
	MaxPosition     float64 `json:"max_position"`
	PositionPercent float64 `json:"position_percent"`

	LiquidationPrice    float64 `json:"liquidation_price"`
	LiquidationDistance float64 `json:"liquidation_distance"`

	RegimeLevel string `json:"regime_level"`

	ShortBoxUpper float64 `json:"short_box_upper"`
	ShortBoxLower float64 `json:"short_box_lower"`
	MidBoxUpper   float64 `json:"mid_box_upper"`
	MidBoxLower   float64 `json:"mid_box_lower"`
	LongBoxUpper  float64 `json:"long_box_upper"`
	LongBoxLower  float64 `json:"long_box_lower"`
	CurrentPrice  float64 `json:"current_price"`

	BreakoutLevel     string `json:"breakout_level"`
	BreakoutDirection string `json:"breakout_direction"`

	// Grid direction
	CurrentGridDirection  string `json:"current_grid_direction"`
	DirectionChangeCount  int    `json:"direction_change_count"`
	EnableDirectionAdjust bool   `json:"enable_direction_adjust"`
}

func (at *AutoTrader) persistGridLedger() error {
	if at.store == nil || at.gridState == nil {
		return nil
	}
	at.gridState.mu.RLock()
	data, err := json.Marshal(at.gridState)
	at.gridState.mu.RUnlock()
	if err == nil {
		err = at.store.PendingEntry().SaveGrid(&store.GridCheckpoint{TraderID: at.id, ExchangeID: at.executionAccountKey(), Symbol: at.config.StrategyConfig.GridConfig.Symbol, StateJSON: string(data), UpdatedAt: time.Now().UTC()})
	}
	if err != nil {
		at.gridState.mu.Lock()
		at.gridState.IsPaused = true
		at.gridState.PauseReason = "ledger persistence failed"
		at.gridState.mu.Unlock()
		logger.Errorf("[Grid] Ledger persistence failed; new entries paused: %v", err)
	}
	return err
}
