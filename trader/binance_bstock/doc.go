// Package binance_bstock executes long-only bStock trades in the USDT spot wallet.
// NewBStockTrader implements types.SpotStockTrader without constructor-time API
// calls. Candidate pairs come only from spot exchangeInfo: TRADING, USDT quote,
// and a base ending in B. Equity-universe classification belongs to the caller.
// Leverage 1 is a no-op; margin modes and shorts are explicitly unsupported.
//
// Account equity includes USDT free+locked and held candidate balances at the
// last price. Wallet balance is equity minus known FIFO unrealized profit.
// Deposits and transfers have unknown cost and contribute no unrealized profit.
// Account, position and history snapshots expire after 15 seconds; own order
// mutations invalidate them, including ambiguous failures. Rules expire after
// ten minutes. Returned maps are copies, and order mutations are serialized.
//
// Orders and OCO lists/legs carry unique nxbs_ client IDs. Protection methods
// reconstruct and replace only tagged SELL protection; CancelAllOrders cancels
// only tagged (program) orders, never the user's manual ones. A lone TP is tagged nxbs_tp_ LIMIT_MAKER. Stop-only
// protection is STOP_LOSS_LIMIT GTC; SetStopLoss defaults its limit to 0.5% below
// the trigger. Canceling one OCO leg cancels the list and replaces the other leg.
// Validation precedes cancellation, but replacement is not atomic: failures
// after cancellation can leave inventory unprotected and require reconciliation.
//
// OCO creation uses POST /api/v3/orderList/oco with explicit above/below types.
// The pinned go-binance v2.8.9 uses the deprecated /api/v3/order/oco and keeps its
// request plumbing private, so a minimal HMAC-SHA256 signer shares the SDK's
// HTTPClient, BaseURL, credentials and TimeOffset. There is no placement retry.
// Open orders are assumed to report orderListId=-1 for lone legs; OCO responses
// must contain numeric list/leg IDs and matching client IDs in orders or
// orderReports. Incomplete placement responses require reconciliation.
//
// FIFO history starts at fromId=0 with 1000-fill pages and extends from the last
// trade ID plus one. State is cached in memory; restarts rebuild from myTrades.
// Buy base-asset commission reduces received units, retaining gross quote cost.
// USDT/BNB buy fees add to cost; sell fees reduce proceeds, or consume additional
// FIFO inventory when paid in base. PnL is net proceeds minus matched cash cost.
// Fee is informational (allocated entry plus exit fees), already included in PnL.
// Base fees convert at the fill price; BNB fees use the BNBUSDT one-minute candle
// close containing that fill, an approximation of the contemporaneous BNB price.
// Missing candles and unknown fee assets return errors. SpotOrderResult retains
// uniform native fee assets and converts mixed assets to USDT; order status and
// realized PnL always report USDT fees. Closed records are per matched sell fill
// with unknown close reason. Account-wide PnL scans even zero-balance candidates,
// but excludes delisted pairs absent from the current TRADING list.
//
// Complete accessible trade history is required; transfers and corporate actions
// are not reconciled. Preflight uses last/average price to estimate percent
// filters; Binance may use another averaging window or reference price. Its
// test-order endpoint is authoritative. Market notional checks use last price.
// External fills and exchange propagation can delay cached fees by 15 seconds.
package binance_bstock
