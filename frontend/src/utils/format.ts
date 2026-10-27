import type { Category, ExpenseStatus, Role } from '@/api/types'

const zeroExponent = new Set([
  'BIF', 'CLP', 'DJF', 'GNF', 'ISK', 'JPY', 'KMF', 'KRW', 'PYG',
  'RWF', 'UGX', 'UYI', 'VND', 'VUV', 'XAF', 'XOF', 'XPF',
])

const threeExponent = new Set([
  'BHD', 'IQD', 'JOD', 'KWD', 'LYD', 'OMR', 'TND',
])

// 与后端 money.Currency.Exponent 保持一致：小数位决定展示与输入换算。
export function currencyExponent(currency: string): number {
  const code = currency.toUpperCase()
  if (zeroExponent.has(code)) return 0
  if (threeExponent.has(code)) return 3
  return 2
}

export function formatMoney(minor: number, currency: string): string {
  const exp = currencyExponent(currency)
  const value = minor / 10 ** exp
  try {
    return new Intl.NumberFormat('zh-CN', {
      style: 'currency',
      currency: currency.toUpperCase(),
      minimumFractionDigits: exp,
      maximumFractionDigits: exp,
    }).format(value)
  } catch {
    // 合法但未知的 ISO 代码在部分浏览器里会抛 RangeError。
    return `${value.toFixed(exp)} ${currency.toUpperCase()}`
  }
}

// 金额只以字符串解析成整数最小单位，绝不经过浮点数。
export function parseMoneyToMinor(raw: string, currency: string): number | null {
  const exp = currencyExponent(currency)
  const text = raw.trim()
  const pattern = exp === 0 ? /^\d+$/ : new RegExp(`^\\d+(\\.\\d{1,${exp}})?$`)
  if (!pattern.test(text)) return null
  const [whole, frac = ''] = text.split('.')
  const minor = Number(whole + (frac + '0'.repeat(exp)).slice(0, exp))
  return Number.isSafeInteger(minor) ? minor : null
}

export function minorToInput(minor: number, currency: string): string {
  const exp = currencyExponent(currency)
  if (exp === 0) return String(minor)
  const text = String(Math.abs(minor)).padStart(exp + 1, '0')
  const value = `${text.slice(0, -exp)}.${text.slice(-exp)}`
  return minor < 0 ? `-${value}` : value
}

export const currencies = ['USD', 'EUR', 'CNY', 'GBP', 'JPY'] as const

export const categoryOptions: { value: Category; label: string }[] = [
  { value: 'travel', label: '差旅' },
  { value: 'meals', label: '餐饮' },
  { value: 'office', label: '办公' },
  { value: 'software', label: '软件' },
  { value: 'other', label: '其他' },
]

const categoryLabels: Record<Category, string> = {
  travel: '差旅',
  meals: '餐饮',
  office: '办公',
  software: '软件',
  other: '其他',
}

export function categoryLabel(category: string): string {
  return categoryLabels[category as Category] ?? category
}

const statusLabels: Record<ExpenseStatus, string> = {
  draft: '草稿',
  submitted: '待审批',
  approved: '已批准',
  rejected: '已驳回',
  cancelled: '已撤销',
  reimbursed: '已报销',
}

export type TagType = 'success' | 'warning' | 'info' | 'danger' | 'primary'

const statusTags: Record<ExpenseStatus, TagType> = {
  draft: 'info',
  submitted: 'warning',
  approved: 'primary',
  rejected: 'danger',
  cancelled: 'info',
  reimbursed: 'success',
}

export function statusLabel(status: string): string {
  return statusLabels[status as ExpenseStatus] ?? status
}

export function statusTagType(status: string): TagType {
  return statusTags[status as ExpenseStatus] ?? 'info'
}

const roleLabels: Record<Role, string> = {
  owner: '所有者',
  admin: '管理员',
  member: '成员',
  viewer: '只读',
}

export function roleLabel(role: string): string {
  return roleLabels[role as Role] ?? role
}

export function formatDate(value: string | null | undefined): string {
  if (!value) return '—'
  if (/^\d{4}-\d{2}-\d{2}$/.test(value)) return value
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

export function formatDateTime(value: string | null | undefined): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${formatDate(value)} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export function isoDate(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

export function daysAgoISO(days: number): string {
  const d = new Date()
  d.setDate(d.getDate() - days)
  return isoDate(d)
}

// 日期选择器只有日期，默认落在当天中午。但如果选的是今天、而现在还没到
// 中午，中午就落在未来，后端（只允许 5 分钟时钟偏差）会以 400 拒绝。
// 当天提前时改用当前时刻；未来日期仍然原样交给后端报错，不静默改期。
export function occurredAtISO(day: string): string {
  const noon = new Date(`${day}T12:00:00`)
  const now = new Date()
  if (noon > now && noon.toDateString() === now.toDateString()) {
    return now.toISOString()
  }
  return noon.toISOString()
}

export function percentOf(part: number, total: number): number {
  if (total <= 0) return 0
  return Math.min(100, Math.round((part / total) * 100))
}
