// Package notify pushes trading events (order fills, risk alerts) to the
// Telegram chat bound in Settings → Telegram. It is intentionally separate
// from the interactive telegram bot package so traders can use it without
// importing the API layer.
//
// Usage: call Init once with the store at startup, then Notify from anywhere.
// Sending is asynchronous with a bounded queue and a simple per-minute rate
// limit so a burst of failures can never block or flood the trading loop.
package notify

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"nofx/logger"
	"nofx/store"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var (
	mu          sync.RWMutex
	st          *store.Store
	bot         *tgbotapi.BotAPI
	chatID      int64
	botToken    string
	lastRefresh time.Time

	queue     chan string
	once      sync.Once
	dropped   int
	droppedMu sync.Mutex
)

const (
	queueSize       = 32
	maxMsgsPerMin   = 20
	refreshInterval = time.Minute
)

// Init wires the store used to resolve the bot token and bound chat.
// Safe to call multiple times; later calls replace the store.
func Init(s *store.Store) {
	mu.Lock()
	st = s
	mu.Unlock()
	once.Do(func() {
		queue = make(chan string, queueSize)
		go senderLoop()
	})
}

// Notify enqueues a trading event for Telegram delivery (non-blocking).
// kind is a short tag like "ORDER" or "ALERT" rendered as the message badge.
// title is the trader name; message is the body. The body may contain HTML
// tags (<b>, <i>, <code>) for rich rendering — escape dynamic values with
// Escape so stray <>& never break Telegram's HTML parsing.
func Notify(kind, title, message string) {
	mu.RLock()
	configured := st != nil
	mu.RUnlock()
	if !configured {
		return
	}

	text := formatMessage(kind, title, message)

	select {
	case queue <- text:
	default:
		droppedMu.Lock()
		dropped++
		droppedMu.Unlock()
	}
}

// Escape escapes the HTML-special characters (& < >) so dynamic values
// (symbols, error strings, reasoning text) can be embedded in rich bodies.
func Escape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

// kindBadge maps a notify kind to its headline emoji.
func kindBadge(kind string) string {
	switch kind {
	case "ORDER":
		return "📊"
	case "RISK":
		return "🛡️"
	case "ALERT":
		return "🚨"
	case "COT":
		return "💭"
	default:
		return "📣"
	}
}

// formatMessage renders the final HTML text: bold badge+trader headline with a
// dim timestamp, then the body (already HTML from the caller).
func formatMessage(kind, title, message string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<b>%s %s</b> · <i>%s</i>\n",
		kindBadge(kind), Escape(title), time.Now().Format("01-02 15:04:05")))
	sb.WriteString(message)
	return sb.String()
}

// sendMessage delivers one message with HTML rendering, falling back to plain
// text when Telegram rejects the markup (a stray < or & in a legacy body must
// never cost us the alert itself).
func sendMessage(text string) bool {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	if _, err := bot.Send(msg); err != nil {
		logger.Warnf("Telegram notify HTML send failed (%v), retrying as plain text", err)
		plain := tgbotapi.NewMessage(chatID, text)
		plain.ParseMode = ""
		if _, err := bot.Send(plain); err != nil {
			logger.Warnf("Telegram notify failed: %v", err)
			return false
		}
	}
	return true
}

// senderLoop is the single delivery goroutine.
func senderLoop() {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	var sent timestamps
	for {
		select {
		case text := <-queue:
			if !ensureClient() {
				continue
			}
			if len(sent) >= maxMsgsPerMin && time.Since(sent[0]) < time.Minute {
				droppedMu.Lock()
				dropped++
				droppedMu.Unlock()
				continue
			}
			if sendMessage(text) {
				sent = append(sent, time.Now())
				trimOld(&sent)
			}
		case <-ticker.C:
			// Re-resolve token/chat so settings changes take effect.
			mu.Lock()
			lastRefresh = time.Time{}
			mu.Unlock()
			ensureClient()
			droppedMu.Lock()
			if dropped > 0 {
				logger.Warnf("Telegram notify: %d messages dropped (queue full or rate limit)", dropped)
				dropped = 0
			}
			droppedMu.Unlock()
		}
	}
}

// ensureClient lazily creates/refreshes the bot client from the DB config.
func ensureClient() bool {
	mu.RLock()
	s := st
	refreshed := lastRefresh
	mu.RUnlock()

	if s == nil {
		return false
	}
	mu.RLock()
	ready := bot != nil && botToken != "" && chatID != 0
	mu.RUnlock()
	if ready && time.Since(refreshed) < refreshInterval {
		return true
	}

	cfg, err := s.TelegramConfig().Get()
	if err != nil || cfg.BotToken == "" {
		return false
	}
	chatIDFromDB, err := s.TelegramConfig().GetBoundChatID()
	if err != nil || chatIDFromDB == 0 {
		return false
	}

	mu.Lock()
	defer mu.Unlock()
	if bot == nil || cfg.BotToken != botToken {
		newBot, err := tgbotapi.NewBotAPI(cfg.BotToken)
		if err != nil {
			logger.Warnf("Telegram notify: bot init failed: %v", err)
			return false
		}
		bot = newBot
		botToken = cfg.BotToken
	}
	chatID = chatIDFromDB
	lastRefresh = time.Now()
	return true
}

// timestamps is a tiny sliding window for rate limiting.
type timestamps []time.Time

func trimOld(ts *timestamps) {
	cutoff := time.Now().Add(-time.Minute)
	keep := (*ts)[:0]
	for _, t := range *ts {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	*ts = keep
}
