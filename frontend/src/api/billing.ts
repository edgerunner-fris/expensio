import { api, type Page } from './client'
import type { Invoice, Plan, Subscription } from './types'

export const billingApi = {
  plans() {
    return api.get<Plan[]>('/billing/plans')
  },
  currentSubscription() {
    return api.get<Subscription>('/billing/subscription')
  },
  listSubscriptions(page = 1, pageSize = 20) {
    return api.get<Page<Subscription>>('/billing/subscriptions', {
      params: { page, page_size: pageSize },
    })
  },
  subscribe(planCode: string, seats: number) {
    return api.post<Subscription>('/billing/subscriptions', {
      plan_code: planCode,
      seats,
    })
  },
  cancel(subscriptionId: string, version: number) {
    return api.post<Subscription>(`/billing/subscriptions/${subscriptionId}/cancel`, { version })
  },
  generateInvoice(subscriptionId: string) {
    return api.post<Invoice>(`/billing/subscriptions/${subscriptionId}/invoices`)
  },
  listInvoices(status: 'open' | 'paid' | '', page = 1, pageSize = 20) {
    return api.get<Page<Invoice>>('/billing/invoices', {
      params: { status: status || undefined, page, page_size: pageSize },
    })
  },
  getInvoice(id: string) {
    return api.get<Invoice>(`/billing/invoices/${id}`)
  },
}
