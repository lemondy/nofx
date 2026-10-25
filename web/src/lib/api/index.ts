import { traderApi } from './traders'
import { strategyApi } from './strategies'
import { configApi } from './config'
import { dataApi } from './data'
import { telegramApi } from './telegram'
import { reviewApi } from './review'
import { systemApi } from './system'
import { netTrafficApi } from './system'
import { fundsApi } from './funds'

export const api = {
  ...traderApi,
  ...strategyApi,
  ...configApi,
  ...dataApi,
  ...telegramApi,
  ...reviewApi,
  ...systemApi,
  ...netTrafficApi,
  ...fundsApi,
}
