package vergex

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"nofx/logger"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// StartHeadlessRelay drives a real (headless) Chrome through the vergex.trade
// Cloudflare challenge on a timer and re-fetches the endpoints the strategy
// engine consumes, feeding the in-memory relay cache (relay.go). This makes
// vergex data (AI500 coin list, OI ranking, NetFlow) independent of the data
// page being open in a human's browser.
//
// Disable with VERGEX_HEADLESS=off. Set VERGEX_HEADLESS_SHOW=true to run the
// browser headed (useful if Cloudflare tightens headless detection).
func StartHeadlessRelay(ctx context.Context, interval time.Duration) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("VERGEX_HEADLESS")))
	if mode == "off" || mode == "false" || mode == "0" {
		logger.Info("🧭 Vergex headless relay disabled (VERGEX_HEADLESS=off)")
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	go runRelay(ctx, interval)
}

// headlessEndpoints are the strategy-facing vergex endpoints: the AI500 coin
// list, the OI ranking tab (coin sources), and the fund-flow ranking. Price
// rankings are served from Binance directly and no longer need the relay.
func headlessEndpoints() []string {
	return []string{
		"/trending-category?lang=en&key=ai500&limit=100",
		"/trending-crypto?tab=oi&duration=1h&limit=100",
		"/api/v1/data-intelligence/flow/markets?limit=25&window=1h",
	}
}

type relaySession struct {
	browser *rod.Browser
	page    *rod.Page
}

func (s *relaySession) close() {
	if s.page != nil {
		_ = s.page.Close()
		s.page = nil
	}
	if s.browser != nil {
		_ = s.browser.Close()
		s.browser = nil
	}
}

// open launches a fresh browser session and navigates to the trending page so
// the Cloudflare challenge resolves inside a real browser context.
func (s *relaySession) open() error {
	s.close()

	headless := strings.ToLower(os.Getenv("VERGEX_HEADLESS_SHOW")) != "true"
	l := launcher.New().
		Headless(headless).
		Set("disable-blink-features", "AutomationControlled")
	controlURL, err := l.Launch()
	if err != nil {
		s.close()
		return fmt.Errorf("launch browser: %w", err)
	}

	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		s.close()
		return fmt.Errorf("connect browser: %w", err)
	}

	page, err := browser.Page(proto.TargetCreateTarget{URL: "https://vergex.trade/trending"})
	if err != nil {
		s.close()
		return fmt.Errorf("open vergex page: %w", err)
	}
	// Give the Cloudflare challenge time to resolve before the first fetches.
	if err := page.WaitStable(8 * time.Second); err != nil {
		logger.Infof("🧭 Vergex headless: page wait interrupted: %v", err)
	}

	s.browser, s.page = browser, page
	return nil
}

// fetchAll fetches every endpoint in the live page context (origin cookies and
// TLS fingerprint are the real browser's) and feeds the relay cache.
func (s *relaySession) fetchAll() error {
	if s.page == nil {
		if err := s.open(); err != nil {
			return err
		}
	}
	for _, endpoint := range headlessEndpoints() {
		res, err := s.page.Eval(
			`(u) => fetch(u, {headers: {accept: 'application/json'}}).then(r => r.text())`,
			"https://vergex.trade"+endpoint,
		)
		if err != nil {
			return fmt.Errorf("eval %s: %w", endpoint, err)
		}
		body := strings.TrimSpace(res.Value.Str())
		if len(body) == 0 || body[0] != '{' {
			return fmt.Errorf("%s: non-JSON response (challenge page?)", endpoint)
		}
		FeedRelay(endpoint, []byte(body))
	}
	return nil
}

func runRelay(ctx context.Context, interval time.Duration) {
	session := &relaySession{}
	defer session.close()

	attempt := func() bool {
		if err := session.fetchAll(); err != nil {
			logger.Infof("🧭 Vergex headless relay failed: %v — restarting browser session", err)
			session.close()
			return false
		}
		return true
	}

	// First round with a few spaced retries (browser cold start can be slow).
	for i := 0; i < 3; i++ {
		if attempt() {
			logger.Infof("🧭 Vergex headless relay active: %d endpoints refreshed every %s",
				len(headlessEndpoints()), interval)
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attempt()
		}
	}
}
