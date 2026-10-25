package store

import (
	"fmt"
	"nofx/fundsledger"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AccountIncome is shared by traders using the same exchange account.
// Amount is signed in Asset; original types and non-stable assets are retained.
type AccountIncome struct {
	AccountKind string `gorm:"not null;default:live_futures;index:idx_account_income_scope"`
	ID          int64  `gorm:"primaryKey;autoIncrement"`
	ExchangeID  string `gorm:"not null;uniqueIndex:idx_account_income_identity;index:idx_account_income_time"`
	IncomeType  string `gorm:"not null;uniqueIndex:idx_account_income_identity"`
	Category    string `gorm:"not null"`
	Symbol      string
	Asset       string
	Amount      float64
	TimeMS      int64  `gorm:"column:time_ms;not null;uniqueIndex:idx_account_income_identity;index:idx_account_income_time"`
	TranID      string `gorm:"not null;uniqueIndex:idx_account_income_identity"`
	Info        string
}

func (AccountIncome) TableName() string { return "account_income" }

type AccountIncomeStore struct{ db *gorm.DB }

func NewAccountIncomeStore(db *gorm.DB) *AccountIncomeStore { return &AccountIncomeStore{db: db} }
func (s *AccountIncomeStore) initTables() error             { return s.db.AutoMigrate(&AccountIncome{}) }
func (s *AccountIncomeStore) Save(exchangeID string, records []fundsledger.Income) error {
	return s.SaveForKind(exchangeID, fundsledger.LiveFutures, records)
}

func (s *AccountIncomeStore) SaveForKind(exchangeID, kind string, records []fundsledger.Income) error {
	if exchangeID == "" {
		return fmt.Errorf("exchange ID required")
	}
	rows := make([]AccountIncome, 0, len(records))
	for _, r := range records {
		rows = append(rows, AccountIncome{AccountKind: kind, ExchangeID: exchangeID, IncomeType: r.IncomeType, Category: fundsledger.ClassifyIncome(r.IncomeType), Symbol: r.Symbol, Asset: r.Asset, Amount: r.Amount, TimeMS: r.TimeMS, TranID: r.TranID, Info: r.Info})
	}
	if len(rows) == 0 {
		return nil
	}
	return s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "exchange_id"}, {Name: "tran_id"}, {Name: "income_type"}, {Name: "time_ms"}}, DoNothing: true}).CreateInBatches(rows, 100).Error
}
func (s *AccountIncomeStore) LatestTime(exchangeID string) (int64, error) {
	var n int64
	err := s.db.Model(&AccountIncome{}).Where("exchange_id = ?", exchangeID).Select("COALESCE(MAX(time_ms), 0)").Scan(&n).Error
	return n, err
}
func (s *AccountIncomeStore) List(exchangeID string, startMS, endMS int64) ([]fundsledger.Income, error) {
	return s.ListForKind(exchangeID, fundsledger.LiveFutures, startMS, endMS)
}

func (s *AccountIncomeStore) LatestTimeForKind(exchangeID, kind string) (int64, error) {
	var n int64
	err := s.db.Model(&AccountIncome{}).Where("exchange_id = ? AND account_kind = ?", exchangeID, kind).Select("COALESCE(MAX(time_ms), 0)").Scan(&n).Error
	return n, err
}

func (s *AccountIncomeStore) ListForKind(exchangeID, kind string, startMS, endMS int64) ([]fundsledger.Income, error) {
	var rows []AccountIncome
	err := s.db.Where("exchange_id = ? AND account_kind = ? AND time_ms >= ? AND time_ms <= ?", exchangeID, kind, startMS, endMS).Order("time_ms ASC, id ASC").Find(&rows).Error
	out := make([]fundsledger.Income, 0, len(rows))
	for _, r := range rows {
		out = append(out, fundsledger.Income{TranID: r.TranID, IncomeType: r.IncomeType, Category: r.Category, Symbol: r.Symbol, Asset: r.Asset, Amount: r.Amount, TimeMS: r.TimeMS, Info: r.Info})
	}
	return out, err
}
func (s *Store) AccountIncome() *AccountIncomeStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accountIncome == nil {
		s.accountIncome = NewAccountIncomeStore(s.gdb)
	}
	return s.accountIncome
}
