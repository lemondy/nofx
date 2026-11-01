import { create } from 'zustand'
import type { AIModel, Exchange } from '../types'
import { api } from '../lib/api'

interface TradersConfigState {
  // 数据
  allModels: AIModel[]
  allExchanges: Exchange[]
  supportedModels: AIModel[]
  supportedExchanges: Exchange[]

  // 计算属性
  configuredModels: AIModel[]
  configuredExchanges: Exchange[]

  // Actions
  setAllModels: (models: AIModel[]) => void
  setAllExchanges: (exchanges: Exchange[]) => void
  setSupportedModels: (models: AIModel[]) => void
  setSupportedExchanges: (exchanges: Exchange[]) => void

  // 异步加载
  loadConfigs: (user: any, token: string | null) => Promise<void>

  // 重置
  reset: () => void
}

const initialState = {
  allModels: [],
  allExchanges: [],
  supportedModels: [],
  supportedExchanges: [],
  configuredModels: [],
  configuredExchanges: [],
}

export const useTradersConfigStore = create<TradersConfigState>((set, get) => ({
  ...initialState,

  setAllModels: (models) => {
    set({ allModels: models })
    // 更新 configuredModels
    const configuredModels = models.filter((m) => {
      return m.enabled || (m.customApiUrl && m.customApiUrl.trim() !== '')
    })
    set({ configuredModels })
  },

  setAllExchanges: (exchanges) => {
    set({ allExchanges: exchanges })
    // 更新 configuredExchanges
    // 2026-10-28 review A7: e.id 是 UUID（不是 'aster'/'hyperliquid' 这样的
    // 类型标识），必须用 e.exchange_type 判断；后端 SafeExchangeConfig 不返回
    // apiKey/secretKey，相关条件是永假死代码，一并删除。
    const configuredExchanges = exchanges.filter((e) => {
      if (e.exchange_type === 'aster') {
        return !!(e.asterUser && e.asterUser.trim() !== '')
      }
      if (e.exchange_type === 'hyperliquid') {
        return !!(
          e.hyperliquidWalletAddr && e.hyperliquidWalletAddr.trim() !== ''
        )
      }
      return e.enabled
    })
    set({ configuredExchanges })
  },

  setSupportedModels: (models) => set({ supportedModels: models }),
  setSupportedExchanges: (exchanges) => set({ supportedExchanges: exchanges }),

  loadConfigs: async (user, token) => {
    if (!user || !token) {
      // 未登录时只加载公开的支持模型和交易所
      try {
        const [supportedModels, supportedExchanges] = await Promise.all([
          api.getSupportedModels(),
          api.getSupportedExchanges(),
        ])
        get().setSupportedModels(supportedModels)
        get().setSupportedExchanges(supportedExchanges)
      } catch (err) {
        console.error('Failed to load supported configs:', err)
      }
      return
    }

    try {
      const [
        modelConfigs,
        exchangeConfigs,
        supportedModels,
        supportedExchanges,
      ] = await Promise.all([
        api.getModelConfigs(),
        api.getExchangeConfigs(),
        api.getSupportedModels(),
        api.getSupportedExchanges(),
      ])

      get().setAllModels(modelConfigs)
      get().setAllExchanges(exchangeConfigs)
      get().setSupportedModels(supportedModels)
      get().setSupportedExchanges(supportedExchanges)
    } catch (error) {
      console.error('Failed to load configs:', error)
    }
  },

  reset: () => set(initialState),
}))
