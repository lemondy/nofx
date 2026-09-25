package trader

import (
	"encoding/json"
	"fmt"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	notify "nofx/telegram/notify"
	"nofx/trader/binance"
	"nofx/trader/types"
	"sort"
	"strings"
	"time"
)

// runCycle runs one trading cycle (using AI full decision-making)
func (at *AutoTrader) runCycle() error {
	at.callCount++

	logger.Info("\n" + strings.Repeat("=", 70) + "\n")
	logger.Infof("⏰ %s - AI decision cycle #%d", time.Now().Format("2006-01-02 15:04:05"), at.callCount)
	logger.Info(strings.Repeat("=", 70))

	// 0. Check if trader is stopped (early exit to prevent trades after Stop() is called)
	at.isRunningMutex.RLock()
	running := at.isRunning
	at.isRunningMutex.RUnlock()
	if !running {
		logger.Infof("⏹ Trader is stopped, aborting cycle #%d", at.callCount)
		return nil
	}

	// Process limit-entry pending orders first: finalize fills, cancel
	// expired/invalidated ones — their outcome shapes this cycle's context.
	at.processPendingEntries()

	// Volatility-targeted rescale (80/120 band) + rule-based trailing stop.
	at.processVolTargetAndTrailing()

	// Per-cycle protection watchdog: re-place missing SL/TP orders at the
	// recorded plan prices (manual cancels, exchange hiccups, missed legs).
	at.processProtectionWatchdog()

	// Close DB OPEN rows the exchange no longer holds (one-way netting or
	// manual closes orphan them — BTWUSDT 09-21). Reporting-only rows:
	// decisions read the exchange live, this just keeps the books honest.
	at.reconcileOrphanedPositionRows()

	// Create decision record
	record := &store.DecisionRecord{
		ExecutionLog: []string{},
		Success:      true,
	}

	// 0.5 Binance auth/IP rejection: configuration problem, not transient —
	// pause all trading (including AI calls) until the operator fixes it.
	if at.authBlocked {
		logger.Errorf("🚨 [%s] Auth/IP blocked: %s — skipping cycle #%d. Add the logged egress IP to the Binance API key whitelist and restart the trader.",
			at.name, at.authBlockedReason, at.callCount)
		record.ErrorMessage = fmt.Sprintf("Auth/IP blocked: %s", at.authBlockedReason)
		at.saveDecision(record)
		return nil
	}

	// 0.4 Config-drift self-check: the strategy row in the DB must still match
	// what this process loaded at start (a UI save or backend edit otherwise
	// silently diverges from the enforced values — the 09-14 sl_min incident).
	at.checkConfigDrift()

	// 1. Check if trading needs to be stopped
	if time.Now().Before(at.stopUntil) {
		remaining := at.stopUntil.Sub(time.Now())
		logger.Infof("⏸ Risk control: Trading paused, remaining %.0f minutes", remaining.Minutes())
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("Risk control paused, remaining %.0f minutes", remaining.Minutes())
		at.saveDecision(record)
		return nil
	}

	// 2. Reset daily P&L (reset every day)
	if time.Since(at.lastResetTime) > 24*time.Hour {
		at.dailyPnL = 0
		at.lastResetTime = time.Now()
		logger.Info("📅 Daily P&L reset")
	}

	// 4. Collect trading context
	ctx, err := at.buildTradingContext()
	if err != nil {
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("Failed to build trading context: %v", err)
		at.saveDecision(record)
		return fmt.Errorf("failed to build trading context: %w", err)
	}

	// Save equity snapshot independently (decoupled from AI decision, used for drawing profit curve)
	// NOTE: Must be called BEFORE candidate coins check to ensure equity is always recorded
	at.saveEquitySnapshot(ctx)

	// If no candidate coins available, log but do not error
	if len(ctx.CandidateCoins) == 0 {
		logger.Infof("ℹ️  No candidate coins available, skipping this cycle")
		record.Success = true // Not an error, just no candidate coins
		record.ExecutionLog = append(record.ExecutionLog, "No candidate coins available, cycle skipped")
		record.AccountState = store.AccountSnapshot{
			TotalBalance:          ctx.Account.TotalEquity,
			AvailableBalance:      ctx.Account.AvailableBalance,
			TotalUnrealizedProfit: ctx.Account.UnrealizedPnL,
			PositionCount:         ctx.Account.PositionCount,
			InitialBalance:        at.initialBalance,
		}
		at.saveDecision(record)
		return nil
	}

	logger.Info(strings.Repeat("=", 70))
	for _, coin := range ctx.CandidateCoins {
		record.CandidateCoins = append(record.CandidateCoins, coin.Symbol)
	}

	logger.Infof("📊 Account equity: %.2f USDT | Available: %.2f USDT | Positions: %d",
		ctx.Account.TotalEquity, ctx.Account.AvailableBalance, ctx.Account.PositionCount)

	// 5. Use strategy engine to call AI for decision
	logger.Infof("🤖 Requesting AI analysis and decision... [Strategy Engine]")
	aiDecision, err := kernel.GetFullDecisionWithStrategy(ctx, at.mcpClient, at.strategyEngine, "balanced")

	if aiDecision != nil && aiDecision.AIRequestDurationMs > 0 {
		record.AIRequestDurationMs = aiDecision.AIRequestDurationMs
		logger.Infof("⏱️ AI call duration: %.2f seconds", float64(record.AIRequestDurationMs)/1000)
		record.ExecutionLog = append(record.ExecutionLog,
			fmt.Sprintf("AI call duration: %d ms", record.AIRequestDurationMs))
	}

	// Save chain of thought, decisions, and input prompt even if there's an error (for debugging)
	if aiDecision != nil {
		record.SystemPrompt = aiDecision.SystemPrompt // Save system prompt
		record.InputPrompt = aiDecision.UserPrompt
		record.CoTTrace = aiDecision.CoTTrace
		record.RawResponse = aiDecision.RawResponse // Save raw AI response for debugging
		if len(aiDecision.Decisions) > 0 {
			decisionJSON, _ := json.MarshalIndent(aiDecision.Decisions, "", "  ")
			record.DecisionJSON = string(decisionJSON)
		}
	}

	// Record AI charge (track cost regardless of decision outcome)
	if aiDecision != nil && at.store != nil {
		if chargeErr := at.store.AICharge().Record(at.id, at.aiModel, at.config.AIModel); chargeErr != nil {
			logger.Warnf("⚠️ Failed to record AI charge: %v", chargeErr)
		}
	}

	if err != nil {
		at.consecutiveAIFailures++
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("Failed to get AI decision: %v", err)

		// Activate safe mode after 3 consecutive failures
		if at.consecutiveAIFailures >= 3 && !at.safeMode {
			at.safeMode = true
			at.safeModeReason = fmt.Sprintf("AI failed %d consecutive times: %v", at.consecutiveAIFailures, err)
			notify.Notify("ALERT", at.name, fmt.Sprintf("<b>🛡️ 已进入安全模式</b>\nAI 连续失败 %d 次：不再开新仓，现有持仓按原止损保护，AI 恢复后自动解除。\n<i>%s</i>", at.consecutiveAIFailures, notify.Escape(err.Error())))
			logger.Errorf("🛡️ [%s] SAFE MODE ACTIVATED — AI failed %d times in a row. No new positions will be opened. Existing positions are protected with current stop-loss settings.",
				at.name, at.consecutiveAIFailures)
			logger.Errorf("🛡️ [%s] Reason: %v", at.name, err)
			logger.Errorf("🛡️ [%s] Action: Will keep trying AI each cycle. Safe mode auto-deactivates when AI recovers.", at.name)
		}

		// Print system prompt and AI chain of thought (output even with errors for debugging)
		if aiDecision != nil {
			logger.Info("\n" + strings.Repeat("=", 70) + "\n")
			logger.Infof("📋 System prompt (error case)")
			logger.Info(strings.Repeat("=", 70))
			logger.Info(aiDecision.SystemPrompt)
			logger.Info(strings.Repeat("=", 70))

			if aiDecision.CoTTrace != "" {
				logger.Info("\n" + strings.Repeat("-", 70) + "\n")
				logger.Info("💭 AI chain of thought analysis (error case):")
				logger.Info(strings.Repeat("-", 70))
				logger.Info(aiDecision.CoTTrace)
				logger.Info(strings.Repeat("-", 70))
			}
		}

		at.saveDecision(record)

		// In safe mode, don't return error — keep the loop running to retry next cycle
		if at.safeMode {
			logger.Warnf("🛡️ [%s] Safe mode: skipping this cycle, will retry in %v", at.name, at.config.ScanInterval)
			return nil
		}

		return fmt.Errorf("failed to get AI decision: %w", err)
	}

	// AI succeeded — reset failure counter and deactivate safe mode
	if at.consecutiveAIFailures > 0 {
		logger.Infof("✅ [%s] AI recovered after %d consecutive failures", at.name, at.consecutiveAIFailures)
	}
	at.consecutiveAIFailures = 0
	if at.safeMode {
		logger.Infof("🛡️ [%s] SAFE MODE DEACTIVATED — AI is working again. Resuming normal trading.", at.name)
		at.safeMode = false
		at.safeModeReason = ""
	}

	// // 5. Print system prompt
	// logger.Infof("\n" + strings.Repeat("=", 70))
	// logger.Infof("📋 System prompt [template: %s]", at.systemPromptTemplate)
	// logger.Info(strings.Repeat("=", 70))
	// logger.Info(decision.SystemPrompt)
	// logger.Infof(strings.Repeat("=", 70) + "\n")

	// 6. Print AI chain of thought
	// logger.Infof("\n" + strings.Repeat("-", 70))
	// logger.Info("💭 AI chain of thought analysis:")
	// logger.Info(strings.Repeat("-", 70))
	// logger.Info(decision.CoTTrace)
	// logger.Infof(strings.Repeat("-", 70) + "\n")

	// 7. Print AI decisions
	// logger.Infof("📋 AI decision list (%d items):\n", len(kernel.Decisions))
	// for i, d := range kernel.Decisions {
	//     logger.Infof("  [%d] %s: %s - %s", i+1, d.Symbol, d.Action, d.Reasoning)
	//     if d.Action == "open_long" || d.Action == "open_short" {
	//        logger.Infof("      Leverage: %dx | Position: %.2f USDT | Stop loss: %.4f | Take profit: %.4f",
	//           d.Leverage, d.PositionSizeUSD, d.StopLoss, d.TakeProfit)
	//     }
	// }
	logger.Info()
	logger.Info(strings.Repeat("-", 70))
	// 8. Sort decisions: ensure close positions first, then open positions (prevent position stacking overflow)
	logger.Info(strings.Repeat("-", 70))

	// 8. Sort decisions: ensure close positions first, then open positions (prevent position stacking overflow)
	sortedDecisions := sortDecisionsByPriority(aiDecision.Decisions)

	logger.Info("🔄 Execution order (optimized): Close positions first → Open positions later")
	for i, d := range sortedDecisions {
		logger.Infof("  [%d] %s %s", i+1, d.Symbol, d.Action)
	}
	logger.Info()

	// Check if trader is stopped before executing any decisions (prevent trades after Stop())
	at.isRunningMutex.RLock()
	running = at.isRunning
	at.isRunningMutex.RUnlock()
	if !running {
		logger.Infof("⏹ Trader stopped before decision execution, aborting cycle #%d", at.callCount)
		return nil
	}

	// Safe mode: filter out open positions, only allow close/hold
	if at.safeMode {
		filtered := make([]kernel.Decision, 0)
		for _, d := range sortedDecisions {
			if d.Action == "open_long" || d.Action == "open_short" || d.Action == "open_long_limit" || d.Action == "open_short_limit" {
				logger.Warnf("🛡️ [%s] Safe mode: BLOCKED %s %s (no new positions allowed)", at.name, d.Action, d.Symbol)
				continue
			}
			filtered = append(filtered, d)
		}
		sortedDecisions = filtered
		if len(sortedDecisions) == 0 {
			logger.Infof("🛡️ [%s] Safe mode: all decisions were open positions, nothing to execute", at.name)
		}
	}

	// Plan-parity: the executor's band/RR checks exempt stops that equal the
	// gated stop_plan — they need this cycle's gate verdicts. Assigned HERE,
	// after the prompt build: the map is created lazily inside
	// computeCoinSignal, and assigning it inside buildTradingContext (before
	// the prompt existed) captured nil — the exemption then never fired and
	// plan-equal stops were rejected at the anchor basis all night
	// (2026-09-20 AVAXUSDT/STRKUSDT/XMRUSDT).
	at.cycleGateStates = ctx.GateStates

	// Shadow-record blocked directions with a complete would-be trade, and
	// resolve matured ones against the price path (09-21 user directive:
	// gate-threshold calibration data). Observational only.
	at.recordGateShadowBlocks(ctx.GateStates, at.callCount)
	at.evaluateGateShadowBlocks()

	// Pre-trade rule check: evaluate review-derived rules before execution.
	// Hard rules with action=block reject the decision outright.
	sortedDecisions = at.preTradeRuleCheck(sortedDecisions, ctx.Account.TotalEquity)

	// Hard risk gates the AI cannot override: 1d-uptrend short block and the
	// minimum holding period lock on closes (both strategy risk_control driven).
	at.cycleRiskReservedUSD = 0 // fresh batch — the reservation accumulates as opens pass the gate
	at.seedAIManagedOnce()      // one-time migration: pre-registry positions presumed AI-managed
	sortedDecisions = at.applyHardRiskGates(sortedDecisions, ctx)
	if len(sortedDecisions) == 0 {
		logger.Infof("🛡️ [%s] All decisions filtered by hard risk gates", at.name)
	}

	// Execute decisions and record results
	for _, d := range sortedDecisions {
		// Check if trader is stopped before each decision (allow immediate stop during execution)
		at.isRunningMutex.RLock()
		running = at.isRunning
		at.isRunningMutex.RUnlock()
		if !running {
			logger.Infof("⏹ Trader stopped during decision execution, aborting remaining decisions")
			break
		}

		actionRecord := store.DecisionAction{
			Action:     d.Action,
			Symbol:     d.Symbol,
			Quantity:   0,
			Leverage:   d.Leverage,
			Price:      0,
			StopLoss:   d.StopLoss,
			TakeProfit: d.TakeProfit,
			Confidence: d.Confidence,
			Reasoning:  d.Reasoning,
			Timestamp:  time.Now().UTC(),
			Success:    false,
		}

		if err := at.executeDecisionWithRecord(&d, &actionRecord); err != nil {
			// R8 release: an open that FAILED execution must give back the
			// stop-risk it booked at the exposure gate — otherwise a failed
			// order keeps eating the account's risk budget for the cycle.
			if strings.HasPrefix(d.Action, "open_") && d.CycleReservedRiskUSD > 0 {
				at.cycleRiskReservedUSD -= d.CycleReservedRiskUSD
				if at.cycleRiskReservedUSD < 0 {
					at.cycleRiskReservedUSD = 0
				}
			}
			logger.Infof("❌ Failed to execute decision (%s %s): %v", d.Symbol, d.Action, err)
			// Alert dedup (CAPUSDT 09-15): the AI retrying an illegal
			// adjust_stop_loss every cycle must not page six times — one alert
			// per (action, symbol) within the gateNotifyRecord window, follow-ups
			// stay in the log with a running streak.
			streak, push := at.gateNotifyRecord("execfail:"+d.Action+":"+d.Symbol, time.Now())
			if push {
				notify.Notify("ALERT", at.name, fmt.Sprintf("<b>❌ %s %s 执行失败</b>\n<code>%s</code>\n<i>同因告警 30 分钟内已去重(streak %d)</i>", notify.Escape(d.Symbol), d.Action, notify.Escape(err.Error()), streak))
			} else {
				logger.Infof("🔇 [%s] 执行失败告警去重(%s %s, streak %d)", at.name, d.Symbol, d.Action, streak)
			}
			// A Binance auth/IP rejection during order execution pauses the
			// trader immediately — retrying orders with a rejected key is noise.
			if binance.IsAuthOrIPError(err) && !at.authBlocked {
				at.authBlocked = true
				at.authBlockedReason = err.Error()
				notify.Notify("ALERT", at.name, "<b>🚨 Binance 认证/IP 校验失败，交易已暂停</b>\n请把日志中的出口 IP 加入 API Key 白名单后重启交易器。")
			}
			actionRecord.Error = err.Error()
			record.ExecutionLog = append(record.ExecutionLog, fmt.Sprintf("❌ %s %s failed: %v", d.Symbol, d.Action, err))
		} else {
			actionRecord.Success = true
			record.ExecutionLog = append(record.ExecutionLog, fmt.Sprintf("✓ %s %s succeeded", d.Symbol, d.Action))
			// Fill notification is handled centrally by the order-sync layer
			// (covers exchange-side stops and manual closes too).
			// Brief delay after successful execution
			time.Sleep(1 * time.Second)
		}

		record.Decisions = append(record.Decisions, actionRecord)

		// Quality→outcome dataset: one row per decision, executed or not.
		if at.store != nil {
			direction := "none"
			switch {
			case d.Action == "open_long" || d.Action == "open_long_limit" || d.Action == "close_short":
				direction = "long"
			case d.Action == "open_short" || d.Action == "open_short_limit" || d.Action == "close_long":
				direction = "short"
			case d.WaitBias == "long" || d.WaitBias == "short":
				direction = d.WaitBias
			}
			quality := -1
			if d.EntryQuality != nil {
				quality = *d.EntryQuality
			}
			mgmtQuality := -1
			if d.ManagementQuality != nil {
				mgmtQuality = *d.ManagementQuality
			}
			// Dataset hygiene (09-15): LOSS_STREAK_BAN is program truth —
			// the model may cite it only for symbols the circuit breaker has
			// actually banned. Strip self-invented bans (it tagged just-won
			// symbols with this 125×/5h during the 09-15 audit) so the
			// quality→win-rate dataset stays trustworthy.
			blockingFactors := d.BlockingFactors
			for _, f := range d.BlockingFactors {
				if f != "LOSS_STREAK_BAN" {
					continue
				}
				if _, banned := ctx.LossStreakBanned[market.Normalize(d.Symbol)]; !banned {
					filtered := make([]string, 0, len(d.BlockingFactors))
					for _, tag := range d.BlockingFactors {
						if tag != "LOSS_STREAK_BAN" {
							filtered = append(filtered, tag)
						}
					}
					blockingFactors = filtered
					logger.Warnf("🧹 [%s] %s: stripped model-declared LOSS_STREAK_BAN (program verdict: not banned) from %v", at.name, d.Symbol, d.BlockingFactors)
				}
				break
			}
			// The rr_scan ceiling shown to the model this cycle for this row's
			// direction (09-16 point 2: makes the wait→fill RR decay measurable).
			gateRR, gateUsable := 0.0, false
			if c := ctx.RRCeilings[market.Normalize(d.Symbol)]; c != nil {
				switch direction {
				case "long":
					gateRR, gateUsable = c.LongRR, c.LongUsable
				case "short":
					gateRR, gateUsable = c.ShortRR, c.ShortUsable
				}
			}
			if err := at.store.EntryAssessment().Insert(&store.EntryAssessment{
				TraderID: at.id, Cycle: at.cycleNumber, Ts: time.Now().UTC(),
				Symbol: d.Symbol, Direction: direction, Action: d.Action,
				Stage: d.Stage, WaitBias: d.WaitBias, EntryQuality: quality,
				WaitState:       d.WaitState,
				NextTrigger:     d.NextTrigger, // clamped to the dataset column width at validation
				GateRR:          gateRR,
				GateUsable:      gateUsable,
				BlockingFactors: store.MarshalBlockingFactors(blockingFactors),
				MgmtQuality:     mgmtQuality,
				MgmtFlags:       store.MarshalBlockingFactors(d.ManagementFlags),
				EntryPath:       actionRecord.EntryPath,
				Price:           d.Price,
			}); err != nil {
				logger.Infof("⚠️ [%s] entry assessment insert failed (%s): %v", at.name, d.Symbol, err)
			}
		}
	}

	// Chain-of-thought push: whenever the AI proposed at least one actionable
	// decision, send its reasoning to Telegram with per-decision outcomes —
	// all-wait cycles stay silent to keep the chat signal-dense.
	if cotHasActionable(aiDecision.Decisions) {
		summaries := make([]notify.DecisionSummary, 0, len(aiDecision.Decisions))
		for _, s := range buildCoTSummaries(aiDecision.Decisions, record.Decisions) {
			s.Detail = cotClamp(s.Detail, 120)
			s.ErrText = cotClamp(s.ErrText, 200)
			summaries = append(summaries, s)
		}
		notify.SendCoT(notify.FormatCoT(at.name, at.callCount, at.aiModel, aiDecision.CoTTrace, summaries))
	}

	// 9. Save decision record
	if err := at.saveDecision(record); err != nil {
		logger.Infof("⚠ Failed to save decision record: %v", err)
	}

	// 10. Sync trade journal (create review entries for newly closed positions)
	if at.store != nil {
		if created, err := at.store.TradeJournal().SyncFromPositions(at.id); err != nil {
			logger.Infof("⚠ [%s] Failed to sync trade journal: %v", at.name, err)
		} else if created > 0 {
			logger.Infof("📓 [%s] Trade journal synced: %d new entries pending review", at.name, created)
		}
	}

	return nil
}

// buildTradingContext builds trading context
func (at *AutoTrader) buildTradingContext() (*kernel.Context, error) {
	// 1. Get account information
	balance, err := at.trader.GetBalance()
	if err != nil {
		if binance.IsAuthOrIPError(err) && !at.authBlocked {
			at.authBlocked = true
			at.authBlockedReason = err.Error()
			notify.Notify("ALERT", at.name, "<b>🚨 Binance 认证/IP 校验失败，交易已暂停</b>\n请把日志中的出口 IP 加入 API Key 白名单后重启交易器。")
		}
		return nil, fmt.Errorf("failed to get account balance: %w", err)
	}

	// Get account fields
	totalWalletBalance := 0.0
	totalUnrealizedProfit := 0.0
	availableBalance := 0.0
	totalEquity := 0.0

	if wallet, ok := balance["totalWalletBalance"].(float64); ok {
		totalWalletBalance = wallet
	}
	if unrealized, ok := balance["totalUnrealizedProfit"].(float64); ok {
		totalUnrealizedProfit = unrealized
	}
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Use totalEquity directly if provided by trader (more accurate)
	if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
		totalEquity = eq
	} else {
		// Fallback: Total Equity = Wallet balance + Unrealized profit
		totalEquity = totalWalletBalance + totalUnrealizedProfit
	}

	// 2. Get position information
	positions, err := at.trader.GetPositions()
	if err != nil {
		if binance.IsAuthOrIPError(err) && !at.authBlocked {
			at.authBlocked = true
			at.authBlockedReason = err.Error()
			notify.Notify("ALERT", at.name, "<b>🚨 Binance 认证/IP 校验失败，交易已暂停</b>\n请把日志中的出口 IP 加入 API Key 白名单后重启交易器。")
		}
		return nil, fmt.Errorf("failed to get positions: %w", err)
	}

	var positionInfos []kernel.PositionInfo
	totalMarginUsed := 0.0

	// Protective SL/TP trigger prices come from the exchange's open orders
	// (authoritative: reflects trailing-stop moves and survives restarts,
	// unlike the in-memory positionStopLoss/TP maps)
	protectionOrderCache := make(map[string][]types.OpenOrder)

	// Current position key set (for cleaning up closed position records)
	currentPositionKeys := make(map[string]bool)

	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := pos["side"].(string)
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity // Short position quantity is negative, convert to positive
		}

		// Skip closed positions (quantity = 0), prevent "ghost positions" from being passed to AI
		if quantity == 0 {
			continue
		}

		unrealizedPnl := pos["unRealizedProfit"].(float64)
		liquidationPrice := pos["liquidationPrice"].(float64)

		// Calculate margin used (initial margin, entry-price basis — the
		// standard ROI denominator; mark-price drift made the same position
		// report a moving margin).
		leverage := 10 // Default value, should actually be fetched from position info
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}
		marginUsed := (quantity * entryPrice) / float64(leverage)
		totalMarginUsed += marginUsed

		// Calculate P&L percentage (based on margin, considering leverage)
		pnlPct := calculatePnLPercentage(unrealizedPnl, marginUsed)
		// ② price return (no leverage) vs margin ROI — two different numbers,
		// both rendered so the model never has to guess the definition.
		priceReturnPct := 0.0
		if entryPrice > 0 {
			if side == "long" {
				priceReturnPct = (markPrice - entryPrice) / entryPrice * 100
			} else {
				priceReturnPct = (entryPrice - markPrice) / entryPrice * 100
			}
		}

		// Get position open time from exchange (preferred) or fallback to local tracking
		posKey := symbol + "_" + side
		currentPositionKeys[posKey] = true

		var updateTime int64
		// Priority 1: Get from database (trader_positions table) - most accurate
		if at.store != nil {
			if dbPos, err := at.store.Position().GetOpenPositionBySymbol(at.id, symbol, side); err == nil && dbPos != nil {
				if dbPos.EntryTime > 0 {
					updateTime = dbPos.EntryTime
				}
			}
		}
		// Priority 2: Get from exchange API (Bybit: createdTime, OKX: createdTime)
		if updateTime == 0 {
			if createdTime, ok := pos["createdTime"].(int64); ok && createdTime > 0 {
				updateTime = createdTime
			}
		}
		// Priority 3: Fallback to local tracking
		if updateTime == 0 {
			if _, exists := at.positionFirstSeenTime[posKey]; !exists {
				at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()
				// First sighting of this posKey — any cached peak belongs to
				// an earlier incarnation (e.g. an externally opened trade
				// reusing the symbol); drop it so the monitor re-seeds fresh.
				at.ClearPeakPnLCache(symbol, side)
				at.unmarkAIManaged(symbol, side) // position gone — registry clean
			}
			updateTime = at.positionFirstSeenTime[posKey]
		}

		// Backfill the gate-facing first-seen map from the authoritative
		// sources too (audit 09-13 #6): the min-hold/early-close gates read
		// positionFirstSeenTime directly, and it was previously only written
		// in the local-fallback branch above — after a restart a pre-existing
		// position with an accurate DB EntryTime left the map empty, which
		// fail-opens both close gates ("age unknown — don't block") for that
		// position's entire life. Seeding is idempotent: the map entry
		// written at open time is never overwritten here.
		if updateTime > 0 {
			if _, exists := at.positionFirstSeenTime[posKey]; !exists {
				at.positionFirstSeenTime[posKey] = updateTime
			}
		}

		// Get peak profit rate for this position
		at.peakPnLCacheMutex.RLock()
		peakPnlPct := at.peakPnLCache[posKey]
		at.peakPnLCacheMutex.RUnlock()

		// Look up the SL/TP orders currently protecting this position on the exchange
		stopLossPrice, takeProfitPrice := 0.0, 0.0
		orders, cached := protectionOrderCache[symbol]
		if !cached {
			orders, err = at.trader.GetOpenOrders(symbol)
			if err != nil {
				logger.Infof("⚠️ [%s] Failed to get open orders for %s: %v", at.name, symbol, err)
			}
			protectionOrderCache[symbol] = orders
		}
		posSideUpper := strings.ToUpper(side) // "LONG"/"SHORT"
		for _, o := range orders {
			if o.PositionSide != "" && o.PositionSide != posSideUpper {
				continue
			}
			switch o.Type {
			case "STOP_MARKET", "STOP":
				if o.StopPrice > 0 {
					stopLossPrice = o.StopPrice
				}
			case "TAKE_PROFIT_MARKET", "TAKE_PROFIT":
				if o.StopPrice > 0 {
					takeProfitPrice = o.StopPrice
				}
			}
		}

		managed := at.isAIManaged(symbol, side)

		positionInfos = append(positionInfos, kernel.PositionInfo{
			Symbol:           symbol,
			Side:             side,
			EntryPrice:       entryPrice,
			MarkPrice:        markPrice,
			Quantity:         quantity,
			Leverage:         leverage,
			UnrealizedPnL:    unrealizedPnl,
			UnrealizedPnLPct: pnlPct,
			PriceReturnPct:   priceReturnPct,
			PeakPnLPct:       peakPnlPct,
			LiquidationPrice: liquidationPrice,
			MarginUsed:       marginUsed,
			UpdateTime:       updateTime,
			StopLossPrice:    stopLossPrice,
			TakeProfitPrice:  takeProfitPrice,
			Managed:          managed,
		})
	}

	// Clean up closed position records
	for key := range at.positionFirstSeenTime {
		if !currentPositionKeys[key] {
			delete(at.positionFirstSeenTime, key)
			// Drop the recorded stop-loss along with the position
			at.positionStopLossMutex.Lock()
			delete(at.positionStopLoss, key)
			delete(at.positionInitialStopLoss, key)
			at.positionStopLossMutex.Unlock()
			// And the peak-PnL cache — otherwise the next position on the
			// same symbol_side inherits a dead trade's peak.
			at.peakPnLCacheMutex.Lock()
			delete(at.peakPnLCache, key)
			at.peakPnLCacheMutex.Unlock()
			// R6 (2026-09-26 review): THIS is the position-gone branch — the
			// AI-mark belongs here, not in the first-sighting branch.
			parts := strings.SplitN(key, "_", 2)
			if len(parts) == 2 {
				at.unmarkAIManaged(parts[0], parts[1])
			}
		}
	}

	// 3. Use strategy engine to get candidate coins (must have strategy engine)
	var candidateCoins []kernel.CandidateCoin
	if at.strategyEngine == nil {
		logger.Infof("⚠️ [%s] No strategy engine configured, skipping candidate coins", at.name)
	} else {
		coins, err := at.strategyEngine.GetCandidateCoins()
		if err != nil {
			// Log warning but don't fail - equity snapshot should still be saved
			logger.Infof("⚠️ [%s] Failed to get candidate coins: %v (will use empty list)", at.name, err)
		} else {
			candidateCoins = coins
			logger.Infof("📋 [%s] Strategy engine fetched candidate coins: %d", at.name, len(candidateCoins))
		}
	}

	// 4. Calculate total P&L
	// Available margin is DERIVED (equity − Σposition margin), not the
	// exchange availableBalance: Binance withholds margin for orders and
	// buffers this prompt never sees (09-18 audit #2: exchange printed
	// 52.62 available against 77.32 equity + 13.94 position margin —
	// unreconcilable, and 32% low for any model-side sizing arithmetic).
	availableBalance = totalEquity - totalMarginUsed
	totalPnL := totalEquity - at.initialBalance
	totalPnLPct := 0.0
	if at.initialBalance > 0 {
		totalPnLPct = (totalPnL / at.initialBalance) * 100
	}

	marginUsedPct := 0.0
	if totalEquity > 0 {
		marginUsedPct = (totalMarginUsed / totalEquity) * 100
	}

	// 5. Get leverage from strategy config
	strategyConfig := at.strategyEngine.GetConfig()
	btcEthLeverage := strategyConfig.RiskControl.BTCETHMaxLeverage
	altcoinLeverage := strategyConfig.RiskControl.AltcoinMaxLeverage
	logger.Infof("📋 [%s] Strategy leverage config: BTC/ETH=%dx, Altcoin=%dx", at.name, btcEthLeverage, altcoinLeverage)

	// 6. Build context
	ctx := &kernel.Context{
		CurrentTime:     time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		RuntimeMinutes:  int(time.Since(at.startTime).Minutes()),
		CallCount:       at.callCount,
		BTCETHLeverage:  btcEthLeverage,
		AltcoinLeverage: altcoinLeverage,
		// Baseline for the account-breaker state shown in the account line.
		InitialBalanceUSDT: at.initialBalance,
		Account: kernel.AccountInfo{
			TotalEquity:      totalEquity,
			AvailableBalance: availableBalance,
			UnrealizedPnL:    totalUnrealizedProfit,
			TotalPnL:         totalPnL,
			TotalPnLPct:      totalPnLPct,
			MarginUsed:       totalMarginUsed,
			MarginUsedPct:    marginUsedPct,
			PositionCount:    len(positionInfos),
		},
		Positions:      positionInfos,
		SymbolStats:    at.loadSymbolStats(),
		CandidateCoins: candidateCoins,
	}
	// NOTE: cycleGateStates is NOT assigned here — ctx.GateStates is created
	// lazily during the prompt build (computeCoinSignal); it is wired into
	// the executor after GetFullDecisionWithStrategy returns, before the
	// decision loop.

	// Loss-streak circuit breaker: program-computed ban map (one store pass),
	// mirrored into each symbol's snapshot so the model reads the verdict
	// instead of self-judging it (09-15 label-abuse fix). Nil = nothing banned.
	if strategyConfig.RiskControl.LossStreakBanEnabled {
		maxLosses := strategyConfig.RiskControl.LossStreakMaxLosses
		if maxLosses <= 0 {
			maxLosses = lossStreakDefaultN
		}
		ctx.LossStreakBanned = at.lossStreakBannedMap(maxLosses)
		if len(ctx.LossStreakBanned) > 0 {
			names := make([]string, 0, len(ctx.LossStreakBanned))
			for sym, until := range ctx.LossStreakBanned {
				names = append(names, fmt.Sprintf("%s(until %s)", sym, until.Local().Format("01-02 15:04")))
			}
			sort.Strings(names)
			logger.Infof("🛡️ [%s] Loss-streak ban active: %s", at.name, strings.Join(names, ", "))
		}
	}

	// 6.5 Inject review-derived trading rules into AI context
	if at.store != nil {
		if rules, err := at.store.Rule().GetEnabledRules(at.id); err == nil && len(rules) > 0 {
			ctx.RulesText = kernel.BuildRulesPromptText(rules)
			hardCount := 0
			for _, r := range rules {
				if r.RuleType == "hard" {
					hardCount++
				}
			}
			logger.Infof("📏 [%s] Injected %d review rules into AI context (%d hard, %d soft)",
				at.name, len(rules), hardCount, len(rules)-hardCount)
		}
	}

	// 7. Add recent closed trades (if store is available)
	if at.store != nil {
		// Get recent 5 closed trades for AI context (09-19 audit 八: per-symbol
		// trader_history + the aggregate stats already cover it; the long tail
		// was ~400 chars of review-only detail)
		recentTrades, err := at.store.Position().GetRecentTrades(at.id, 5)
		if err != nil {
			logger.Infof("⚠️ [%s] Failed to get recent trades: %v", at.name, err)
		} else {
			logger.Infof("📊 [%s] Found %d recent closed trades for AI context", at.name, len(recentTrades))
			for _, trade := range recentTrades {
				// Convert Unix timestamps to formatted strings for AI readability
				entryTimeStr := ""
				if trade.EntryTime > 0 {
					entryTimeStr = time.Unix(trade.EntryTime, 0).UTC().Format("01-02 15:04 UTC")
				}
				exitTimeStr := ""
				if trade.ExitTime > 0 {
					exitTimeStr = time.Unix(trade.ExitTime, 0).UTC().Format("01-02 15:04 UTC")
				}

				ctx.RecentOrders = append(ctx.RecentOrders, kernel.RecentOrder{
					Symbol:        trade.Symbol,
					Side:          trade.Side,
					EntryPrice:    trade.EntryPrice,
					ExitPrice:     trade.ExitPrice,
					RealizedPnL:   trade.RealizedPnL,
					PnLPct:        trade.PnLPct,
					PositionValue: trade.PositionValue,
					Fee:           trade.Fee,
					EntryTime:     entryTimeStr,
					ExitTime:      exitTimeStr,
					HoldDuration:  trade.HoldDuration,
				})
			}
		}
		// Get trading statistics for AI context. Rolling window (config
		// stats_window_days, default 30; negative = full history): only
		// trades closed inside the window feed PF/win-rate/expectancy, so a
		// strategy that improved is not permanently dragged down by ancient
		// losses. Nothing is deleted — the window is a query-time filter.
		statsWindow := strategyConfig.EffectiveStatsWindowDays()
		windowLabel := fmt.Sprintf("%dd window", statsWindow)
		if statsWindow <= 0 {
			windowLabel = "full history"
		}
		stats, err := at.store.Position().GetRollingStats(at.id, at.initialBalance, statsWindow)
		if err != nil {
			logger.Infof("⚠️ [%s] Failed to get trading stats: %v", at.name, err)
		} else if stats == nil {
			logger.Infof("⚠️ [%s] GetRollingStats returned nil", at.name)
		} else if stats.TotalTrades == 0 {
			logger.Infof("⚠️ [%s] Trading stats: 0 closed trades in %s (traderID=%s) — strategy_health omitted", at.name, windowLabel, at.id)
		} else {
			maxDD := stats.MaxDrawdownPct
			// Prefer the real equity curve when available: it includes
			// unrealized swings that closed-trade PnL never shows. The
			// curve is clipped to the same window so DD and PF describe
			// the same period.
			snapLimit := 2000
			if statsWindow > 0 {
				// ~5min snapshot cadence (288/day) plus headroom.
				snapLimit = statsWindow*288 + 100
			}
			if snaps, err := at.store.Equity().GetLatest(at.id, snapLimit); err == nil && len(snaps) > 1 {
				if statsWindow > 0 {
					cutoff := time.Now().Add(-time.Duration(statsWindow) * 24 * time.Hour)
					filtered := snaps[:0]
					for _, sn := range snaps {
						if sn.Timestamp.After(cutoff) {
							filtered = append(filtered, sn)
						}
					}
					snaps = filtered
				}
				if len(snaps) > 1 {
					peak := snaps[0].TotalEquity
					var dd float64
					for _, sn := range snaps {
						if sn.TotalEquity > peak {
							peak = sn.TotalEquity
						}
						if peak > 0 {
							if d := (peak - sn.TotalEquity) / peak * 100; d > dd {
								dd = d
							}
						}
					}
					if dd > 0 {
						maxDD = dd
					}
				}
			}
			ctx.TradingStats = &kernel.TradingStats{
				TotalTrades:    stats.TotalTrades,
				WinRate:        stats.WinRate,
				ProfitFactor:   stats.ProfitFactor,
				SharpeRatio:    stats.SharpeRatio,
				TotalPnL:       stats.TotalPnL,
				AvgWin:         stats.AvgWin,
				AvgLoss:        stats.AvgLoss,
				MaxDrawdownPct: maxDD,
				WindowDays:     stats.WindowDays,
			}
			// Measured R distribution (E1, QUANT_REVIEW 09-22): expectancy in
			// R from journal rows with a planned stop, same window — replaces
			// the −1R-loser assumption when there is enough data.
			var sinceMs int64
			if stats.WindowDays > 0 {
				sinceMs = time.Now().UTC().AddDate(0, 0, -stats.WindowDays).UnixMilli()
			}
			if rm, err := at.store.TradeJournal().MeasureRExpectancy(at.id, sinceMs); err != nil {
				logger.Infof("⚠️ [%s] measured R expectancy unavailable: %v", at.name, err)
			} else if rm.Samples >= kernel.MinMeasuredRSamples {
				ctx.TradingStats.MeasuredAvgWinR = rm.AvgWinR
				ctx.TradingStats.MeasuredAvgLossR = rm.AvgLossR
				ctx.TradingStats.MeasuredExpectancyR = rm.ExpectancyR
				ctx.TradingStats.MeasuredRSamples = rm.Samples
				logger.Infof("📈 [%s] Measured R (%s): %d trades, avgWin %+.2fR, avgLoss %.2fR, expectancy %+.2fR",
					at.name, windowLabel, rm.Samples, rm.AvgWinR, rm.AvgLossR, rm.ExpectancyR)
			}
			logger.Infof("📈 [%s] Trading stats (%s): %d trades, %.1f%% win rate, PF=%.2f, Sharpe=%.2f, DD=%.1f%%",
				at.name, windowLabel, stats.TotalTrades, stats.WinRate, stats.ProfitFactor, stats.SharpeRatio, stats.MaxDrawdownPct)
		}
	} else {
		logger.Infof("⚠️ [%s] Store is nil, cannot get recent trades", at.name)
	}

	// 8. Get quantitative data (if enabled in strategy config)
	if strategyConfig.Indicators.EnableQuantData {
		// Collect symbols to query (candidate coins + position coins)
		symbolsToQuery := make(map[string]bool)
		for _, coin := range candidateCoins {
			symbolsToQuery[coin.Symbol] = true
		}
		for _, pos := range positionInfos {
			symbolsToQuery[pos.Symbol] = true
		}

		symbols := make([]string, 0, len(symbolsToQuery))
		for sym := range symbolsToQuery {
			symbols = append(symbols, sym)
		}

		logger.Infof("📊 [%s] Fetching quantitative data for %d symbols...", at.name, len(symbols))
		ctx.QuantDataMap = at.strategyEngine.FetchQuantDataBatch(symbols)
		logger.Infof("📊 [%s] Successfully fetched quantitative data for %d symbols", at.name, len(ctx.QuantDataMap))
	}

	// 9. Get OI ranking data (market-wide position changes)
	if strategyConfig.Indicators.EnableOIRanking {
		logger.Infof("📊 [%s] Fetching OI ranking data...", at.name)
		ctx.OIRankingData = at.strategyEngine.FetchOIRankingData()
		if ctx.OIRankingData != nil {
			logger.Infof("📊 [%s] OI ranking data ready: %d top, %d low positions",
				at.name, len(ctx.OIRankingData.TopPositions), len(ctx.OIRankingData.LowPositions))
		}
	}

	// 10. Get NetFlow ranking data (market-wide fund flow)
	if strategyConfig.Indicators.EnableNetFlowRanking {
		logger.Infof("💰 [%s] Fetching NetFlow ranking data...", at.name)
		ctx.NetFlowRankingData = at.strategyEngine.FetchNetFlowRankingData()
		if ctx.NetFlowRankingData != nil {
			logger.Infof("💰 [%s] NetFlow ranking data ready: inst_in=%d, inst_out=%d",
				at.name, len(ctx.NetFlowRankingData.InstitutionFutureTop), len(ctx.NetFlowRankingData.InstitutionFutureLow))
		}
	}

	// 11. Get Price ranking data (market-wide gainers/losers)
	if strategyConfig.Indicators.EnablePriceRanking {
		logger.Infof("📈 [%s] Fetching Price ranking data...", at.name)
		ctx.PriceRankingData = at.strategyEngine.FetchPriceRankingData()
		if ctx.PriceRankingData != nil {
			logger.Infof("📈 [%s] Price ranking data ready for %d durations",
				at.name, len(ctx.PriceRankingData.Durations))
		}
	}

	return ctx, nil
}

// sortDecisionsByPriority sorts decisions: close positions first, then open positions, finally hold/wait
// This avoids position stacking overflow when changing positions
func sortDecisionsByPriority(decisions []kernel.Decision) []kernel.Decision {
	if len(decisions) <= 1 {
		return decisions
	}

	// Define priority
	getActionPriority := func(action string) int {
		switch action {
		case "close_long", "close_short":
			return 1 // Highest priority: close positions first
		case "open_long", "open_short", "open_long_limit", "open_short_limit":
			return 2 // Second priority: open positions later
		case "hold", "wait":
			return 3 // Lowest priority: wait
		default:
			return 999 // Unknown actions at the end
		}
	}

	// Copy decision list
	sorted := make([]kernel.Decision, len(decisions))
	copy(sorted, decisions)

	// Sort by priority
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if getActionPriority(sorted[i].Action) > getActionPriority(sorted[j].Action) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	return sorted
}

// loadSymbolStats converts the store's per-symbol closed-trade record into the
// kernel-side map injected into every symbol's signal block.
func (at *AutoTrader) loadSymbolStats() map[string]*kernel.TraderHistoryStat {
	if at.store == nil {
		return nil
	}
	stats, err := at.store.Position().GetSymbolStats(at.id, 200)
	if err != nil {
		logger.Infof("⚠️ [%s] Failed to load symbol stats: %v", at.name, err)
		return nil
	}
	out := make(map[string]*kernel.TraderHistoryStat, len(stats))
	for i := range stats {
		s := stats[i]
		out[strings.ToUpper(s.Symbol)] = &kernel.TraderHistoryStat{
			ClosedTrades: s.TotalTrades,
			Wins:         s.WinTrades,
			WinRatePct:   s.WinRate,
			RealizedPnL:  s.TotalPnL,
		}
	}
	return out
}
