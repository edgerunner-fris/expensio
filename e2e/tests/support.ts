import { expect, type Page } from '@playwright/test'

export interface TenantAccount {
  slug: string
  name: string
  email: string
  password: string
  displayName: string
}

// 每次运行注册全新租户：E2E 写真实数据库，固定值会在重复运行时撞唯一键。
export function uniqueAccount(prefix: string): TenantAccount {
  const runId = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`
  const slug = `${prefix}-${runId}`.toLowerCase()
  return {
    slug,
    name: `E2E ${runId}`,
    email: `${slug}@example.com`,
    password: `e2e-${runId}-pass`,
    displayName: 'E2E 用户',
  }
}

export async function registerTenant(page: Page, account: TenantAccount): Promise<void> {
  await page.goto('/register')
  await page.getByPlaceholder('例如 acme', { exact: true }).fill(account.slug)
  await page.getByPlaceholder('例如 Acme 科技', { exact: true }).fill(account.name)
  await page.getByPlaceholder('例如 李明').fill(account.displayName)
  await page.getByPlaceholder('you@company.com').fill(account.email)
  const passwords = page.locator('input[type="password"]')
  await passwords.nth(0).fill(account.password)
  await passwords.nth(1).fill(account.password)
  await page.getByRole('button', { name: '创建并登录' }).click()
  await page.waitForURL('**/expenses')
  await expect(page.getByRole('heading', { name: '费用单' })).toBeVisible()
}

// 邀请链接的域名由部署配置决定（容器内是 localhost:5173，浏览器里访问不到），
// 所以 E2E 只取路径，再交给 Playwright 的 baseURL 拼成真实可达地址。
export function invitePath(link: string): string {
  return link.startsWith('http') ? new URL(link).pathname : link
}
