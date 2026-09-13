package kernel

// ============================================================================
// Trading Data Schema
// ============================================================================
// Bilingual data dictionary supporting Chinese and English.
// Ensures AI can fully understand data formats regardless of language.
// ============================================================================

const (
	SchemaVersion = "1.0.0"
)

// Language represents the language type
type Language string

const (
	LangChinese Language = "zh-CN"
	LangEnglish Language = "en-US"
)

// ========== Bilingual Field Definitions ==========

// BilingualFieldDef defines a field with bilingual name, formula, and description
type BilingualFieldDef struct {
	NameZH    string // Chinese name
	NameEN    string // English name
	Unit      string // unit of measurement
	FormulaZH string // Chinese formula
	FormulaEN string // English formula
	DescZH    string // Chinese description
	DescEN    string // English description
}

// GetName returns the field name based on language
func (d BilingualFieldDef) GetName(lang Language) string {
	if lang == LangChinese {
		return d.NameZH
	}
	return d.NameEN
}

// GetFormula returns the formula based on language
func (d BilingualFieldDef) GetFormula(lang Language) string {
	if lang == LangChinese {
		return d.FormulaZH
	}
	return d.FormulaEN
}

// GetDesc returns the description based on language
func (d BilingualFieldDef) GetDesc(lang Language) string {
	if lang == LangChinese {
		return d.DescZH
	}
	return d.DescEN
}

// ========== Data Dictionary ==========

// DataDictionary defines the meaning of all fields
var DataDictionary = map[string]map[string]BilingualFieldDef{
	"AccountMetrics": {
		"Equity": {
			NameZH:    "总权益",
			NameEN:    "Total Equity",
			Unit:      "USDT",
			FormulaZH: "可用余额 + 已用保证金 + 未实现盈亏（= 钱包余额 + 未实现盈亏）",
			FormulaEN: "Available Balance + Used Margin + Unrealized PnL (= Wallet Balance + Unrealized PnL)",
			DescZH:    "账户的实际净值，包含所有持仓的浮动盈亏",
			DescEN:    "Actual account value including all unrealized P&L from positions",
		},
		"Balance": {
			NameZH:    "可用余额",
			NameEN:    "Available Balance",
			Unit:      "USDT",
			FormulaZH: "总权益 - 未实现盈亏 - 已用保证金（= 钱包余额 - 已用保证金）",
			FormulaEN: "Total Equity - Unrealized PnL - Used Margin (= Wallet Balance - Used Margin)",
			DescZH:    "可用于开新仓位的保证金。开仓占用保证金、持仓浮亏都会使其减少；它不是初始资金加已实现盈亏，也不包含已用保证金",
			DescEN:    "Margin available for opening new positions. Decreases when positions lock margin or float at a loss; NOT initial capital + realized PnL, and excludes used margin",
		},
		"PnL": {
			NameZH:    "总盈亏百分比",
			NameEN:    "Total PnL Percentage",
			Unit:      "%",
			FormulaZH: "(总权益 - 初始资金) / 初始资金 × 100",
			FormulaEN: "(Total Equity - Initial Capital) / Initial Capital × 100",
			DescZH:    "自系统启动以来的总收益率，+15.87%表示盈利15.87%",
			DescEN:    "Total return since inception, +15.87% means 15.87% profit",
		},
		"Margin": {
			NameZH:    "保证金使用率",
			NameEN:    "Margin Usage Rate",
			Unit:      "%",
			FormulaZH: "已用保证金合计 / 总权益 × 100",
			FormulaEN: "Total Used Margin / Total Equity × 100",
			DescZH:    "该值越高，账户风险越大。安全值<30%，危险值>70%",
			DescEN:    "Higher value = higher risk. Safe <30%, Dangerous >70%",
		},
	},

	"TradeMetrics": {
		"Entry": {
			NameZH: "进场价",
			NameEN: "Entry Price",
			Unit:   "USDT",
			DescZH: "开仓时的平均价格",
			DescEN: "Average price when opening position",
		},
		"Exit": {
			NameZH: "出场价",
			NameEN: "Exit Price",
			Unit:   "USDT",
			DescZH: "平仓时的平均价格",
			DescEN: "Average price when closing position",
		},
		"Profit": {
			NameZH:    "已实现盈亏",
			NameEN:    "Realized PnL",
			Unit:      "USDT",
			FormulaZH: "(出场价 - 进场价) / 进场价 × 杠杆 × 仓位价值",
			FormulaEN: "(Exit Price - Entry Price) / Entry Price × Leverage × Position Value",
			DescZH:    "已平仓交易的实际盈亏，包含手续费。正值=盈利，负值=亏损",
			DescEN:    "Actual profit/loss of closed trades including fees. Positive=profit, Negative=loss",
		},
		"PnL%": {
			NameZH:    "盈亏百分比",
			NameEN:    "PnL Percentage",
			Unit:      "%",
			FormulaZH: "(出场价 - 进场价) / 进场价 × 杠杆 × 100",
			FormulaEN: "(Exit - Entry) / Entry × Leverage × 100",
			DescZH:    "已平仓交易的收益率，+6.71%表示盈利6.71%",
			DescEN:    "Return on closed trade, +6.71% means 6.71% profit",
		},
		"HoldDuration": {
			NameZH: "持仓时长",
			NameEN: "Holding Duration",
			Unit:   "minutes",
			DescZH: "从开仓到平仓的时间。<15分钟=超短线，15分钟-4小时=日内，>4小时=波段",
			DescEN: "Time from open to close. <15min=scalping, 15min-4h=intraday, >4h=swing",
		},
	},

	"PositionMetrics": {
		"UnrealizedPnL%": {
			NameZH:    "未实现盈亏百分比",
			NameEN:    "Unrealized PnL Percentage",
			Unit:      "%",
			FormulaZH: "(当前价 - 进场价) / 进场价 × 杠杆 × 100",
			FormulaEN: "(Current Price - Entry Price) / Entry Price × Leverage × 100",
			DescZH:    "当前持仓的浮动盈亏，未平仓前是浮动的",
			DescEN:    "Floating P&L of current position, not realized until closed",
		},
		"PeakPnL%": {
			NameZH: "峰值盈亏百分比",
			NameEN: "Peak PnL Percentage",
			Unit:   "%",
			DescZH: "该持仓曾经达到的最高未实现盈亏。用于判断是否需要止盈",
			DescEN: "Historical max unrealized PnL for this position. Used for take-profit decisions",
		},
		"Drawdown": {
			NameZH:    "从峰值回撤",
			NameEN:    "Drawdown from Peak",
			Unit:      "%",
			FormulaZH: "当前盈亏% - 峰值盈亏%",
			FormulaEN: "Current PnL% - Peak PnL%",
			DescZH:    "负值表示正在回撤。例如：峰值+5%，当前+3%，回撤=-2%",
			DescEN:    "Negative = pulling back. E.g., Peak +5%, Current +3%, Drawdown = -2%",
		},
		"Leverage": {
			NameZH: "杠杆倍数",
			NameEN: "Leverage",
			Unit:   "x",
			DescZH: "3x表示价格变动1%，持仓盈亏变动3%。杠杆越高，风险越大",
			DescEN: "3x means 1% price move = 3% position PnL. Higher leverage = higher risk",
		},
		"Margin": {
			NameZH:    "占用保证金",
			NameEN:    "Margin Used",
			Unit:      "USDT",
			FormulaZH: "仓位价值 / 杠杆",
			FormulaEN: "Position Value / Leverage",
			DescZH:    "该仓位锁定的保证金金额",
			DescEN:    "Collateral locked for this position",
		},
		"LiqPrice": {
			NameZH: "强平价格",
			NameEN: "Liquidation Price",
			Unit:   "USDT",
			DescZH: "价格触及此值时会被强制平仓。0.0000表示无爆仓风险",
			DescEN: "Price at which position will be force-closed. 0.0000 = no liquidation risk",
		},
		"StopLossPrice": {
			NameZH: "止损挂单",
			NameEN: "SL Order",
			Unit:   "USDT",
			DescZH: "该仓位当前挂在交易所的保护性止损单触发价（实时查询，含程序 1R 保本/跟踪止损及 AI adjust_stop_loss 后的最新价）。未挂单=该仓位当前无保护，应视为风险缺口；需要改止损价时用 adjust_stop_loss 动作",
			DescEN: "Trigger price of the protective stop-loss order currently on the exchange (live query, reflects trailing-stop moves). none = position is unprotected, treat as a risk gap",
		},
		"TakeProfitPrice": {
			NameZH: "止盈挂单",
			NameEN: "TP Order",
			Unit:   "USDT",
			DescZH: "该仓位当前挂在交易所的保护性止盈单触发价（实时查询）。未挂单=当前无止盈保护",
			DescEN: "Trigger price of the protective take-profit order currently on the exchange (live query). none = no take-profit protection",
		},
	},

	"MarketData": {
		"Volume": {
			NameZH: "成交量",
			NameEN: "Volume",
			Unit:   "base asset",
			DescZH: "该时间段的交易量",
			DescEN: "Trading volume in this period",
		},
		"OI": {
			NameZH: "持仓量",
			NameEN: "Open Interest",
			Unit:   "USDT",
			DescZH: "未平仓合约的总价值。持仓量增加=资金流入，减少=资金流出",
			DescEN: "Total value of open contracts. Increasing OI = capital inflow, decreasing = outflow",
		},
		"OIChange": {
			NameZH: "持仓量变化",
			NameEN: "OI Change",
			Unit:   "USDT & %",
			DescZH: "1小时内持仓量的变化。用于判断市场真实资金流向",
			DescEN: "OI change in 1 hour. Used to determine real capital flow direction",
		},
	},
}

// ========== OI Interpretation ==========

// OIInterpretation defines bilingual market interpretations for OI changes
type OIInterpretationType struct {
	OIUp_PriceUp struct {
		ZH string
		EN string
	}
	OIUp_PriceDown struct {
		ZH string
		EN string
	}
	OIDown_PriceUp struct {
		ZH string
		EN string
	}
	OIDown_PriceDown struct {
		ZH string
		EN string
	}
}

var OIInterpretation = OIInterpretationType{
	OIUp_PriceUp: struct {
		ZH string
		EN string
	}{
		ZH: "强多头趋势（新多单开仓，资金流入做多）",
		EN: "Strong bullish trend (new longs opening, capital flowing into long positions)",
	},
	OIUp_PriceDown: struct {
		ZH string
		EN string
	}{
		ZH: "强空头趋势（新空单开仓，资金流入做空）",
		EN: "Strong bearish trend (new shorts opening, capital flowing into short positions)",
	},
	OIDown_PriceUp: struct {
		ZH string
		EN string
	}{
		ZH: "空头平仓（空头止损离场，可能出现反转）",
		EN: "Shorts covering (shorts stopped out, potential reversal)",
	},
	OIDown_PriceDown: struct {
		ZH string
		EN string
	}{
		ZH: "多头平仓（多头止损离场，可能出现反转）",
		EN: "Longs closing (longs stopped out, potential reversal)",
	},
}

// ========== Prompt Generation Functions ==========

// GetSchemaPrompt generates schema description text for AI prompts
func GetSchemaPrompt(lang Language) string {
	if lang == LangChinese {
		return getSchemaPromptZH()
	}
	return getSchemaPromptEN()
}

// getSchemaPromptZH generates the Chinese prompt
func getSchemaPromptZH() string {
	prompt := "# 📖 数据字典与交易规则\n\n"
	prompt += "## 📊 字段含义说明\n\n"

	// Account metrics
	prompt += "### 账户指标\n"
	for key, field := range DataDictionary["AccountMetrics"] {
		prompt += formatFieldDefZH(key, field)
	}

	// Trade metrics
	prompt += "\n### 交易指标\n"
	for key, field := range DataDictionary["TradeMetrics"] {
		prompt += formatFieldDefZH(key, field)
	}

	// Position metrics
	prompt += "\n### 持仓指标\n"
	for key, field := range DataDictionary["PositionMetrics"] {
		prompt += formatFieldDefZH(key, field)
	}

	// Market data
	prompt += "\n### 市场数据\n"
	for key, field := range DataDictionary["MarketData"] {
		prompt += formatFieldDefZH(key, field)
	}

	// OI interpretation
	prompt += "\n## 💹 持仓量(OI)变化解读\n\n"
	prompt += "- **OI增加 + 价格上涨**: " + OIInterpretation.OIUp_PriceUp.ZH + "\n"
	prompt += "- **OI增加 + 价格下跌**: " + OIInterpretation.OIUp_PriceDown.ZH + "\n"
	prompt += "- **OI减少 + 价格上涨**: " + OIInterpretation.OIDown_PriceUp.ZH + "\n"
	prompt += "- **OI减少 + 价格下跌**: " + OIInterpretation.OIDown_PriceDown.ZH + "\n"

	return prompt
}

// getSchemaPromptEN generates the English prompt
func getSchemaPromptEN() string {
	prompt := "# 📖 Data Dictionary & Trading Rules\n\n"
	prompt += "## 📊 Field Definitions\n\n"

	// Account Metrics
	prompt += "### Account Metrics\n"
	for key, field := range DataDictionary["AccountMetrics"] {
		prompt += formatFieldDefEN(key, field)
	}

	// Trade Metrics
	prompt += "\n### Trade Metrics\n"
	for key, field := range DataDictionary["TradeMetrics"] {
		prompt += formatFieldDefEN(key, field)
	}

	// Position Metrics
	prompt += "\n### Position Metrics\n"
	for key, field := range DataDictionary["PositionMetrics"] {
		prompt += formatFieldDefEN(key, field)
	}

	// Market Data
	prompt += "\n### Market Data\n"
	for key, field := range DataDictionary["MarketData"] {
		prompt += formatFieldDefEN(key, field)
	}

	// OI Interpretation
	prompt += "\n## 💹 Open Interest (OI) Change Interpretation\n\n"
	prompt += "- **OI Up + Price Up**: " + OIInterpretation.OIUp_PriceUp.EN + "\n"
	prompt += "- **OI Up + Price Down**: " + OIInterpretation.OIUp_PriceDown.EN + "\n"
	prompt += "- **OI Down + Price Up**: " + OIInterpretation.OIDown_PriceUp.EN + "\n"
	prompt += "- **OI Down + Price Down**: " + OIInterpretation.OIDown_PriceDown.EN + "\n"

	return prompt
}

// formatFieldDefZH formats a field definition in Chinese
func formatFieldDefZH(key string, field BilingualFieldDef) string {
	result := "- **" + key + "**（" + field.NameZH + "）: " + field.DescZH
	if field.FormulaZH != "" {
		result += " | 公式: `" + field.FormulaZH + "`"
	}
	if field.Unit != "" {
		result += " | 单位: " + field.Unit
	}
	result += "\n"
	return result
}

// formatFieldDefEN formats a field definition in English
func formatFieldDefEN(key string, field BilingualFieldDef) string {
	result := "- **" + key + "** (" + field.NameEN + "): " + field.DescEN
	if field.FormulaEN != "" {
		result += " | Formula: `" + field.FormulaEN + "`"
	}
	if field.Unit != "" {
		result += " | Unit: " + field.Unit
	}
	result += "\n"
	return result
}
