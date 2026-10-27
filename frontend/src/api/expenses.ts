import { api, type Page } from './client'
import type {
  ApprovalPolicy,
  ApprovalProgress,
  Attachment,
  Budget,
  BudgetInput,
  BudgetUpdateInput,
  DraftInput,
  Expense,
  ExpenseComment,
  PresignUpload,
} from './types'

export interface ExpenseListQuery {
  scope?: 'mine' | 'all'
  status?: string
  // inbox=true 时由服务端返回"待我审批"，忽略 scope 与 status。
  inbox?: boolean
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
  approvals(id: string) {
    return api.get<ApprovalProgress>(`/expenses/${id}/approvals`)
  },
}

export const commentApi = {
  list(expenseID: string) {
    return api.get<{ items: ExpenseComment[] }>(`/expenses/${expenseID}/comments`)
  },
  add(expenseID: string, body: string) {
    return api.post<ExpenseComment>(`/expenses/${expenseID}/comments`, { body })
  },
  remove(expenseID: string, commentID: string) {
    return api.delete<void>(`/expenses/${expenseID}/comments/${commentID}`)
  },
}

export interface AttachmentMeta {
  file_name: string
  content_type: string
  size_bytes: number
}

export const attachmentApi = {
  list(expenseID: string) {
    return api.get<{ items: Attachment[] }>(`/expenses/${expenseID}/attachments`)
  },
  presign(expenseID: string, meta: AttachmentMeta) {
    return api.post<PresignUpload>(`/expenses/${expenseID}/attachments/presign`, meta)
  },
  confirm(expenseID: string, input: AttachmentMeta & { object_key: string }) {
    return api.post<Attachment>(`/expenses/${expenseID}/attachments`, input)
  },
  remove(expenseID: string, attachmentID: string) {
    return api.delete<void>(`/expenses/${expenseID}/attachments/${attachmentID}`)
  },
  // 直传对象存储：走原生 fetch，绝不把 Bearer token 发给第三方域名，
  // 也不经过 axios 的响应拦截器（MinIO 返回的不是业务信封）。
  async upload(presigned: PresignUpload, file: File): Promise<void> {
    const resp = await fetch(presigned.upload_url, {
      method: presigned.method,
      headers: presigned.headers,
      body: file,
    })
    if (!resp.ok) throw new Error(`上传失败（HTTP ${resp.status}）`)
  },
}

export const approvalApi = {
  listPolicies() {
    return api.get<{ items: ApprovalPolicy[] }>('/approval-policies')
  },
  upsertPolicy(currency: string, thresholdMinor: number) {
    return api.put<ApprovalPolicy>(`/approval-policies/${currency}`, {
      second_level_threshold_minor: thresholdMinor,
    })
  },
  removePolicy(currency: string) {
    return api.delete<void>(`/approval-policies/${currency}`)
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
