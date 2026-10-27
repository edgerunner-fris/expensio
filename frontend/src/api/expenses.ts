import { api, type Page } from './client'
import type { Budget, BudgetInput, BudgetUpdateInput, DraftInput, Expense } from './types'

export interface ExpenseListQuery {
  scope?: 'mine' | 'all'
  status?: string
  page?: number
  page_size?: number
}

export const expenseApi = {
  list(query: ExpenseListQuery) {
    return api.get<Page<Expense>>('/expenses', { params: query })
  },
  get(id: string) {
    return api.get<Expense>(`/expenses/${id}`)
  },
  create(input: DraftInput) {
    return api.post<Expense>('/expenses', input)
  },
  update(id: string, input: DraftInput & { version: number }) {
    return api.put<Expense>(`/expenses/${id}`, input)
  },
  remove(id: string, version: number) {
    return api.delete<void>(`/expenses/${id}?version=${version}`)
  },
  action(id: string, action: 'submit' | 'approve' | 'reject' | 'cancel' | 'reimburse', comment?: string) {
    return api.post<Expense>(`/expenses/${id}/${action}`, comment ? { comment } : {})
  },
}

export interface BudgetListQuery {
  category?: string
  page?: number
  page_size?: number
}

export const budgetApi = {
  list(query: BudgetListQuery) {
    return api.get<Page<Budget>>('/budgets', { params: query })
  },
  get(id: string) {
    return api.get<Budget>(`/budgets/${id}`)
  },
  create(input: BudgetInput) {
    return api.post<Budget>('/budgets', input)
  },
  update(id: string, input: BudgetUpdateInput) {
    return api.put<Budget>(`/budgets/${id}`, input)
  },
  remove(id: string, version: number) {
    return api.delete<void>(`/budgets/${id}?version=${version}`)
  },
}
