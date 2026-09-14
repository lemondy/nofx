import { API_BASE, httpClient } from './helpers'

export interface SystemQualityAIStats {
  count: number
  avg_ms?: number
  p50_ms?: number
  p95_ms?: number
  max_ms?: number
  min_ms?: number
}

export interface SystemQualityFailure {
  category: string
  count: number
  sample: string
}

export interface SystemQualityHour {
  hour: string
  cycles: number
  failures: number
  avg_duration_ms: number
}

export interface SystemQuality {
  trader_id: string
  window_hours: number
  total_cycles: number
  successful_cycles: number
  failed_cycles: number
  success_rate_pct: number
  ai_calls: SystemQualityAIStats
  failures: SystemQualityFailure[]
  hourly: SystemQualityHour[]
}

export const systemApi = {
  async getSystemQuality(traderId: string, hours = 24): Promise<SystemQuality> {
    const result = await httpClient.request<SystemQuality>(
      `${API_BASE}/system-quality?trader_id=${encodeURIComponent(traderId)}&hours=${hours}`
    )
    if (!result.success) throw new Error('Failed to fetch system quality')
    return result.data!
  },
}
