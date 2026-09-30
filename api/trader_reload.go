package api

import "nofx/logger"

// Stop joins the old client and persists false. Preserve the pre-reload intent
// so the manager alone starts the replacement exactly once.
func (s *Server) removeTraderForReload(userID, traderID string) {
	cfg, err := s.store.Trader().GetForUser(userID, traderID)
	if err != nil {
		return
	}
	resume := cfg.IsRunning
	full, configErr := s.store.Trader().GetFullConfig(userID, traderID)
	if configErr != nil || full.AIModel == nil || !full.AIModel.Enabled || full.Exchange == nil || !full.Exchange.Enabled {
		resume = false
	}
	if err := s.traderManager.RemoveTraderAndThen(traderID, func() error {
		return s.store.Trader().UpdateStatus(userID, traderID, resume)
	}); err != nil {
		logger.Errorf("Failed to preserve trader reload state: %v", err)
	}
}
