export type Role = 'owner' | 'admin' | 'member' | 'viewer'

export type ExpenseStatus =
  | 'draft'
  | 'submitted'
  | 'approved'
  | 'rejected'
  | 'cancelled'
  | 'reimbursed'

export type Category = 'travel' | 'meals' | 'office' | 'software' | 'other'

export interface Profile {
  user_id: string
  email: string
  display_name: string
  tenant_id: string
  tenant_slug: string
  tenant_name: string
  role: Role
}

export interface Session {
  access_token: string
  access_expires_at: string
  refresh_token: string
  refresh_expires_at: string
  profile: Profile
}

export interface Member {
  user_id: string
  email: string
  display_name: string
  role: Role
  status: string
  created_at: string
}

export interface Invitation {
  id: string
  email: string
  role: Role
  invited_by: string
  expires_at: string
  created_at: string
}

export interface CreatedInvitation extends Invitation {
  // 链接只在创建响应里出现一次，之后无法再取回。
  link: string
  email_queued: boolean
}

export interface InvitationInfo {
  tenant_name: string
  tenant_slug: string
  email: string
  role: Role
  expires_at: string
  account_exists: boolean
}

export interface NotificationItem {
  id: string
  kind: string
  title: string
  body: string
  resource_type?: string
  resource_id?: string
  read_at?: string
  created_at: string
}

export interface AuditEntry {
  id: string
  actor_user_id?: string
  action: string
  target_type?: string
  target_id?: string
  metadata: Record<string, unknown>
  created_at: string
}

export interface Expense {
  id: string
  submitter_id: string
  status: ExpenseStatus
  category: Category
  currency: string
  amount_minor: number
  merchant: string
  description: string
  occurred_at: string
  budget_id?: string
  decided_by?: string
  decided_at?: string
  rejection_reason?: string
  version: number
  // 提交时按金额快照的审批级数；approval_level 是已完成的级数。
  required_approval_levels: number
  approval_level: number
  created_at: string
  updated_at: string
}

export interface ExpenseApproval {
  level: number
  actor_id: string
  comment?: string
  created_at: string
}

export interface ApprovalProgress {
  required_levels: number
  approved_levels: number
  items: ExpenseApproval[]
}

export interface Attachment {
  id: string
  file_name: string
  content_type: string
  size_bytes: number
  uploaded_by: string
  created_at: string
  // 短期签名 URL，每次列表都会重新签发，不能缓存复用。
  download_url: string
  download_expires_at: string
}

export interface PresignUpload {
  object_key: string
  upload_url: string
  method: 'PUT'
  headers: Record<string, string>
  expires_at: string
}

export interface ExpenseComment {
  id: string
  author_id: string
  body: string
  created_at: string
}

export interface ApprovalPolicy {
  currency: string
  second_level_threshold_minor: number
  updated_at: string
}

export interface DraftInput {
  category: Category
  currency: string
  amount_minor: number
  merchant: string
  description: string
  occurred_at: string
  budget_id?: string
}

export interface Budget {
  id: string
  name: string
  category: Category
  currency: string
  period_start: string
  period_end: string
  amount_minor: number
  reserved_minor: number
  spent_minor: number
  available_minor: number
  version: number
  created_at: string
  updated_at: string
}

export interface BudgetInput {
  name: string
  category: Category
  currency: string
  period_start: string
  period_end: string
  amount_minor: number
}

export interface BudgetUpdateInput {
  version: number
  name: string
  period_start: string
  period_end: string
  amount_minor: number
}

export interface CurrencyTotal {
  currency: string
  amount_minor: number
  expense_count: number
}

export interface GroupTotal {
  group: string
  currency: string
  amount_minor: number
  expense_count: number
}

export interface ExpenseSummary {
  scope: 'mine' | 'all'
  group_by: 'category' | 'status'
  from: string
  to: string
  currency_totals: CurrencyTotal[]
  groups: GroupTotal[]
}

export interface Plan {
  code: string
  name: string
  price_minor: number
  currency: string
  interval: 'monthly' | 'yearly'
}

export interface Subscription {
  id: string
  plan_code: string
  seats: number
  status: 'active' | 'past_due' | 'canceled'
  current_period_start: string
  current_period_end: string
  version: number
  canceled_at?: string
  created_at: string
  updated_at: string
}

export interface Invoice {
  id: string
  subscription_id: string
  number: string
  status: 'open' | 'paid'
  period_start: string
  period_end: string
  currency: string
  amount_minor: number
  due_on: string
  issued_at: string
  paid_at?: string
  version: number
}
