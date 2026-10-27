import { api, download } from './client'
import type { Dashboard, ExpenseSummary } from './types'

export interface SummaryQuery {
  from: string
  to: string
  group_by: 'category' | 'status'
  scope?: 'mine' | 'all'
  status?: string
  category?: string
}

export interface DashboardQuery {
  from: string
  to: string
  scope?: 'mine' | 'all'
}

export const reportApi = {
  summary(query: SummaryQuery) {
    return api.get<ExpenseSummary>('/reports/expenses/summary', { params: query })
  },
  dashboard(query: DashboardQuery) {
    return api.get<Dashboard>('/reports/dashboard', { params: query })
  },
  exportCSV(query: SummaryQuery) {
    return download('/reports/expenses.csv', { ...query })
  },
}
