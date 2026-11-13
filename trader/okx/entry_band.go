package okx

// GetSymbolTickSize shares the limit path's cached instrument rules.
func (t *OKXTrader) GetSymbolTickSize(symbol string) (float64, error) {
	rules, err := t.getInstrument(symbol)
	if err != nil {
		return 0, err
	}
	return rules.TickSz, nil
}
