package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"nofx/logger"

	"gorm.io/gorm"
)

type decisionSystemPrompt struct {
	Hash      string    `gorm:"column:hash;type:text;primaryKey"`
	Text      string    `gorm:"column:text;type:text;not null"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (decisionSystemPrompt) TableName() string { return "decision_system_prompts" }

func decisionPromptDedupeEnabled() bool {
	return os.Getenv("NOFX_DECISION_PROMPT_DEDUPE") != "0"
}

// storeDecisionSystemPrompt must be called in the record's write transaction.
// Always read back the text before allowing the caller to clear the inline copy.
func storeDecisionSystemPrompt(tx *gorm.DB, text string) (hash string, matches bool, err error) {
	sum := sha256.Sum256([]byte(text))
	hash = hex.EncodeToString(sum[:])
	query := "INSERT OR IGNORE INTO decision_system_prompts (hash, text, created_at) VALUES (?, ?, ?)"
	if tx.Dialector.Name() == "postgres" {
		query = "INSERT INTO decision_system_prompts (hash, text, created_at) VALUES (?, ?, ?) ON CONFLICT (hash) DO NOTHING"
	}
	if err := tx.Exec(query, hash, text, time.Now().UTC()).Error; err != nil {
		return hash, false, err
	}
	var prompt decisionSystemPrompt
	if err := tx.Where("hash = ?", hash).Take(&prompt).Error; err != nil {
		return hash, false, err
	}
	if prompt.Text != text {
		logger.Errorf("Decision system prompt hash mismatch, keeping inline text (hash=%s)", hash)
		return hash, false, nil
	}
	return hash, true, nil
}

// toRecords resolves distinct hashes with one IN query for the whole result set.
// Inline legacy text always takes precedence, even if a row also has a hash.
func (s *DecisionStore) toRecords(rows []*DecisionRecordDB) ([]*DecisionRecord, error) {
	hashes := make([]string, 0)
	prompts := make(map[string]string)
	for _, row := range rows {
		if row.SystemPrompt == "" && row.SystemPromptHash != "" {
			if _, seen := prompts[row.SystemPromptHash]; !seen {
				hashes = append(hashes, row.SystemPromptHash)
				prompts[row.SystemPromptHash] = ""
			}
		}
	}
	if len(hashes) > 0 {
		var stored []decisionSystemPrompt
		if err := s.db.Where("hash IN ?", hashes).Find(&stored).Error; err != nil {
			return nil, fmt.Errorf("failed to resolve decision system prompts: %w", err)
		}
		// Missing entries remain absent so they can be logged once per store.
		clear(prompts)
		for _, prompt := range stored {
			prompts[prompt.Hash] = prompt.Text
		}
		for _, hash := range hashes {
			if _, found := prompts[hash]; !found {
				s.missingPromptLog.Do(func() {
					logger.Errorf("Decision system prompt missing, returning empty text (hash=%s)", hash)
				})
			}
		}
	}
	records := make([]*DecisionRecord, len(rows))
	for i, row := range rows {
		records[i] = row.toRecord(prompts)
	}
	return records, nil
}

// StartSystemPromptBackfill starts one finite background run after store init.
// The caller owns ctx and should cancel it before closing the database.
func (s *DecisionStore) StartSystemPromptBackfill(ctx context.Context) {
	if !decisionPromptDedupeEnabled() {
		return
	}
	s.promptBackfillStarter.Do(func() {
		go func() {
			if _, err := s.BackfillSystemPromptHashes(ctx, 0, 0); err != nil {
				logger.Errorf("Decision system prompt backfill stopped: %v", err)
			}
		}()
	})
}

// BackfillSystemPromptHashes replaces only verified inline system prompts.
// Non-positive batchSize/pause use 200 rows and 200ms. A cursor advances past
// mismatches and concurrent edits so a finite run cannot loop on skipped rows.
// A later run starts at the beginning and can retry any remaining legacy rows.
// No VACUUM is run. SQLite reuses the pages freed by the smaller records.
func (s *DecisionStore) BackfillSystemPromptHashes(ctx context.Context, batchSize int, pause time.Duration) (migrated int, err error) {
	if !decisionPromptDedupeEnabled() {
		return 0, nil
	}
	if batchSize <= 0 {
		batchSize = 200
	}
	if pause <= 0 {
		pause = 200 * time.Millisecond
	}
	batches, skipped := 0, 0
	distinct := make(map[string]struct{})
	defer func() {
		logger.Infof("Decision system prompt backfill finished (migrated=%d distinct_prompts=%d skipped=%d batches=%d err=%v)", migrated, len(distinct), skipped, batches, err)
	}()
	var lastID int64
	for {
		if err := ctx.Err(); err != nil {
			return migrated, err
		}
		if !decisionPromptDedupeEnabled() {
			return migrated, nil
		}
		var rows []DecisionRecordDB
		var batchMigrated, batchSkipped int
		var batchHashes []string
		err := retryOnceOnBusy(func() error {
			// The whole batch rolls back on BUSY. Count only the committed attempt.
			rows = nil
			batchMigrated, batchSkipped = 0, 0
			batchHashes = nil
			return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				query := tx.Select("id, system_prompt").Where("system_prompt != '' AND system_prompt_hash = ''")
				if batches > 0 {
					query = query.Where("id > ?", lastID)
				}
				if err := query.Order("id ASC").Limit(batchSize).Find(&rows).Error; err != nil {
					return err
				}
				for _, row := range rows {
					hash, matches, err := storeDecisionSystemPrompt(tx, row.SystemPrompt)
					if err != nil {
						return err
					}
					if !matches {
						batchSkipped++
						continue
					}
					result := tx.Exec(`UPDATE decision_records SET system_prompt='', system_prompt_hash=?
						WHERE id=? AND system_prompt=? AND system_prompt_hash=''`, hash, row.ID, row.SystemPrompt)
					if result.Error != nil {
						return result.Error
					}
					if result.RowsAffected == 0 {
						batchSkipped++
						continue
					}
					batchMigrated++
					batchHashes = append(batchHashes, hash)
				}
				return nil
			})
		})
		if err != nil {
			return migrated, fmt.Errorf("failed to backfill decision system prompts: %w", err)
		}
		if len(rows) == 0 {
			return migrated, nil
		}
		lastID = rows[len(rows)-1].ID
		migrated += batchMigrated
		skipped += batchSkipped
		batches++
		for _, hash := range batchHashes {
			distinct[hash] = struct{}{}
		}
		if batches%25 == 0 {
			logger.Infof("Decision system prompt backfill progress (migrated=%d distinct_prompts=%d skipped=%d batches=%d last_id=%d)", migrated, len(distinct), skipped, batches, lastID)
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return migrated, ctx.Err()
		case <-timer.C:
		}
	}
}
