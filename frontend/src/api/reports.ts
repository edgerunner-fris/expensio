import { api } from './client'
import type { ExpenseSummary } from './types'

export interface SummaryQuery {
  from: string
  to: string
  group_by: 'category' | 'status'
  scope?: 'mine' | 'all'
  status?: string
  category?: string
}

export const reportApi = {
  summary(query: SummaryQuery) {
    return api.get<ExpenseSummary>('/reports/expenses/summary', { params: query })
  },
}
