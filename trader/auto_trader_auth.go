package trader

import (
	"fmt"
	"strings"
	"time"

	"nofx/logger"
	"nofx/telegram/notify"
	"nofx/trader/binance"
)

const (
	authProbeMinInterval     = time.Minute
	authBlockedAlertInterval = 30 * time.Minute
)

var authNotify = notify.Notify

func (at *AutoTrader) authTime() time.Time {
	if at.authNow != nil {
		return at.authNow()
	}
	return time.Now()
}

// Initial delay is 1m; failed probes then wait 1m, 2m, 5m, 10m, 10m, ... .
func authProbeDelay(probes int) time.Duration {
	switch {
	case probes <= 1:
		return authProbeMinInterval
	case probes == 2:
		return 2 * time.Minute
	case probes == 3:
		return 5 * time.Minute
	default:
		return 10 * time.Minute
	}
}

func (at *AutoTrader) isAuthBlocked() bool {
	blocked, _ := at.authBlockStatus()
	return blocked
}

func (at *AutoTrader) authBlockStatus() (bool, string) {
	at.authMu.Lock()
	defer at.authMu.Unlock()
	return at.authBlocked, at.authBlockedReason
}

func (at *AutoTrader) markAuthBlocked(err error) {
	if err == nil {
		return
	}
	at.authMu.Lock()
	defer at.authMu.Unlock()
	at.authGeneration++ // a newer rejection must invalidate an in-flight success
	at.authBlockedReason = err.Error()
	if at.authBlocked {
		return
	}
	now := at.authTime()
	at.authBlocked = true
	at.authBlockedSince = now
	at.authNextProbeAt = now.Add(authProbeDelay(0))
	at.authProbeCount = 0
	at.authLastProbeError = ""
	at.authLastAlertAt = now
	authNotify("ALERT", at.name, "<b>🚨 Binance 认证/IP 校验失败，交易已暂停</b>\n"+
		notify.Escape(shortAuthReason(at.authBlockedReason))+"\n请固定代理出口 IP 并把它加入 API Key 白名单；恢复后无需重启，会自动探测恢复。")
}

func (at *AutoTrader) clearAuthBlocked() {
	at.authMu.Lock()
	defer at.authMu.Unlock()
	at.clearAuthBlockedLocked(at.authTime())
}

// Caller holds authMu. Probe completion uses the same reset while checking
// its generation atomically, so an older success cannot clear a newer block.
func (at *AutoTrader) clearAuthBlockedLocked(now time.Time) {
	if !at.authBlocked {
		return
	}
	duration, probes := authBlockedDuration(now.Sub(at.authBlockedSince)), at.authProbeCount
	at.authBlocked = false
	at.authBlockedReason = ""
	at.authBlockedSince = time.Time{}
	at.authNextProbeAt = time.Time{}
	at.authProbeCount = 0
	at.authLastAlertAt = time.Time{}
	at.authLastProbeError = ""
	at.authGeneration++
	logger.Infof("✅ [%s] Binance authentication recovered; trading resumes (blocked %s, %d probes)", at.name, duration, probes)
	authNotify("ALERT", at.name, fmt.Sprintf("✅ Binance 认证已恢复，交易继续（被拦 %s，探测 %d 次）", duration, probes))
}

// Called only by the minute drawdown monitor, under the account execution
// lock. authMu protects scheduling as well, including concurrent test callers.
// No exchange mutation is used to probe: invalidate the local cache, then read
// GetBalance. Release authMu during I/O so state reads/marks never wait on HTTP.
func (at *AutoTrader) probeAuthRecovery() {
	at.authMu.Lock()
	now := at.authTime()
	if !at.authBlocked || at.authProbeInFlight {
		at.authMu.Unlock()
		return
	}
	if now.Before(at.authNextProbeAt) {
		at.remindAuthBlockedLocked(now)
		at.authMu.Unlock()
		return
	}
	at.authProbeInFlight = true
	at.authProbeCount++
	at.authNextProbeAt = now.Add(authProbeDelay(at.authProbeCount))
	generation := at.authGeneration
	at.authMu.Unlock()
	defer func() {
		at.authMu.Lock()
		at.authProbeInFlight = false
		at.authMu.Unlock()
	}()

	if cache, ok := at.trader.(interface{ InvalidateAccountCache() }); ok {
		cache.InvalidateAccountCache()
	}
	_, err := at.trader.GetBalance()

	at.authMu.Lock()
	defer at.authMu.Unlock()
	if !at.authBlocked || generation != at.authGeneration {
		return
	}
	now = at.authTime()
	if err == nil {
		at.clearAuthBlockedLocked(now)
		return
	}
	at.authLastProbeError = err.Error()
	if binance.IsAuthOrIPError(err) {
		at.authBlockedReason = err.Error()
		logger.Warnf("🚨 [%s] Binance auth/IP recovery probe #%d rejected: %v", at.name, at.authProbeCount, err)
	} else {
		logger.Warnf("⚠️ [%s] Binance auth/IP recovery probe #%d inconclusive: %v", at.name, at.authProbeCount, err)
	}
	at.authNextProbeAt = now.Add(authProbeDelay(at.authProbeCount))
	at.remindAuthBlockedLocked(now)
}

// Caller holds authMu. Notify only enqueues; serialize it with state changes
// so reminders cannot be enqueued after a recovery notification.
func (at *AutoTrader) remindAuthBlockedLocked(now time.Time) {
	interval := at.authAlertInterval
	if interval <= 0 {
		interval = authBlockedAlertInterval
	}
	if !at.authBlocked || now.Sub(at.authLastAlertAt) < interval {
		return
	}
	at.authLastAlertAt = now
	reason := shortAuthReason(at.authLastProbeError)
	if reason == "" {
		reason = shortAuthReason(at.authBlockedReason)
	} else if at.authLastProbeError != at.authBlockedReason {
		reason += "；认证/IP 错误：" + shortAuthReason(at.authBlockedReason)
	}
	authNotify("ALERT", at.name, fmt.Sprintf("🚨 Binance 认证/IP 仍被拦（已 %s，最近一次探测失败：%s）。请固定代理出口 IP 并把它加入 API Key 白名单；恢复后无需重启，会自动探测恢复。",
		authBlockedDuration(now.Sub(at.authBlockedSince)), notify.Escape(reason)))
}

func authBlockedDuration(d time.Duration) string {
	minutes := int64(d / time.Minute)
	if minutes < 0 {
		minutes = 0
	}
	return fmt.Sprintf("%d 小时 %d 分", minutes/60, minutes%60)
}

func shortAuthReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	runes := []rune(reason)
	if len(runes) <= 300 {
		return reason
	}
	short := string(runes[:300]) + "…"
	// Binance puts the current request IP near the end of some errors.
	if i := strings.Index(strings.ToLower(reason), "request ip:"); i >= 0 && !strings.Contains(short, reason[i:]) {
		fields := strings.Fields(reason[i:])
		short += " " + strings.Join(fields[:min(3, len(fields))], " ")
	}
	return short
}
