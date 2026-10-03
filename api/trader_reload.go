package api

import "nofx/logger"

// Running intent remains in the database while the old client drains. The
// loader checks its current version, so a stop during reload cannot be undone.
func (s *Server) removeTraderForReload(userID, traderID string) {
	if _, err := s.store.Trader().GetForUser(userID, traderID); err != nil {
		return
	}
	cfg, err := s.store.Trader().GetFullConfig(userID, traderID)
	if err != nil || cfg.AIModel == nil || !cfg.AIModel.Enabled || cfg.Exchange == nil || !cfg.Exchange.Enabled {
		if err := s.store.Trader().UpdateStatus(userID, traderID, false); err != nil {
			logger.Errorf("disable reload: %v", err)
			return
		}
	}
	if err := s.traderManager.RemoveTraderAndThen(traderID, nil); err != nil {
		logger.Errorf("trader reload: %v", err)
	}
}
