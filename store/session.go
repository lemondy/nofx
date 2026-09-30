package store

import (
	"crypto/sha256"
	"encoding/hex"
	"gorm.io/gorm/clause"
	"time"
)

type RevokedToken struct {
	Hash      string    `gorm:"primaryKey"`
	ExpiresAt time.Time `gorm:"index"`
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) RevokeToken(token string, expires time.Time) error {
	return s.gdb.Clauses(clause.OnConflict{DoNothing: true}).Create(&RevokedToken{Hash: tokenHash(token), ExpiresAt: expires}).Error
}

func (s *Store) TokenRevoked(token string) (bool, error) {
	var count int64
	err := s.gdb.Model(&RevokedToken{}).Where("hash = ? AND expires_at > ?", tokenHash(token), time.Now().UTC()).Count(&count).Error
	return count > 0, err
}
