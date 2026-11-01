import type {
  HistoricalPosition,
  MaxDrawdownBasis,
  TraderStats,
} from '../../types'

const isNum = (v: unknown): v is number =>
  typeof v === 'number' && Number.isFinite(v)

// Net PnL of one closed position: the backend net_pnl (includes funding) when
// present, otherwise the legacy realized_pnl − fee.
export function positionNetPnl(
  p: Pick<HistoricalPosition, 'realized_pnl' | 'fee' | 'net_pnl'>
): number {
  if (isNum(p.net_pnl)) return p.net_pnl
  return (p.realized_pnl || 0) - (p.fee || 0)
}

// Σ net PnL over a list of positions.
export function sumNetPnl(
  positions: Array<Pick<HistoricalPosition, 'realized_pnl' | 'fee' | 'net_pnl'>>
): number {
  return positions.reduce((sum, p) => sum + positionNetPnl(p), 0)
}

// Headline net PnL of the stats block: total_net_pnl (includes funding) when
// present, otherwise the legacy total_pnl.
export function statsNetPnl(
  s: Pick<TraderStats, 'total_pnl' | 'total_net_pnl'>
): number {
  if (isNum(s.total_net_pnl)) return s.total_net_pnl
  return s.total_pnl || 0
}

// Drawdown basis normalised to a known value; undefined when the backend did
// not send one (older backend) or sent something unrecognised.
export function drawdownBasis(
  s: Pick<TraderStats, 'max_drawdown_basis'>
): MaxDrawdownBasis | undefined {
  const b = s.max_drawdown_basis
  return b === 'equity_twr' || b === 'closed_trades' || b === 'unavailable'
    ? b
    : undefined
}
