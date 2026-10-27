import { expect, test, type Page } from '@playwright/test'

// 每次运行都注册一个全新租户，避免依赖上一次的数据库状态。
const runId = Date.now().toString(36)
const tenantSlug = `e2e-${runId}`
const tenantName = `E2E ${runId}`
const email = `${tenantSlug}@example.com`
const password = `e2e-${runId}-pass`
const merchant = `E2E 商户 ${runId}`

// 报表是事件驱动的最终一致读模型：提交后事件要先经 outbox → Kafka → report
// 消费者落库。这里轮询 API 等读模型追上，而不是靠固定 sleep。
async function waitForReportData(page: Page): Promise<void> {
  await expect
    .poll(
      async () => {
        const token = await page.evaluate(() => localStorage.getItem('expensio.access_token'))
        // 用 UTC 前后各一天做窗口，避免浏览器时区与 UTC 跨日导致漏查。
        const [from, to] = await page.evaluate(() => {
          const iso = (d: Date) => d.toISOString().slice(0, 10)
          const now = new Date()
          const prev = new Date(now)
          prev.setUTCDate(prev.getUTCDate() - 1)
          const next = new Date(now)
          next.setUTCDate(next.getUTCDate() + 1)
          return [iso(prev), iso(next)]
        })
        const resp = await page.request.get(
          `/api/v1/reports/expenses/summary?from=${from}&to=${to}&group_by=category&scope=all`,
          { headers: { Authorization: `Bearer ${token}` } },
        )
        if (!resp.ok()) return 0
        const body = (await resp.json()) as {
          data?: { currency_totals?: unknown[] }
        }
        return body.data?.currency_totals?.length ?? 0
      },
      { timeout: 20_000, message: '等待报表读模型消费费用事件' },
    )
    .toBeGreaterThan(0)
}

async function registerTenant(page: Page): Promise<void> {
  await page.goto('/register')
  await page.getByPlaceholder('例如 acme', { exact: true }).fill(tenantSlug)
  await page.getByPlaceholder('例如 Acme 科技', { exact: true }).fill(tenantName)
  await page.getByPlaceholder('例如 李明').fill('E2E 用户')
  await page.getByPlaceholder('you@company.com').fill(email)
  const passwords = page.locator('input[type="password"]')
  await passwords.nth(0).fill(password)
  await passwords.nth(1).fill(password)
  await page.getByRole('button', { name: '创建并登录' }).click()
  await page.waitForURL('**/expenses')
}

test('未登录访问受保护路由会跳到登录页', async ({ page }) => {
  await page.goto('/expenses')
  await expect(page).toHaveURL(/\/login\?redirect=/)
  await expect(page.getByRole('heading', { name: '登录' })).toBeVisible()
})

test('注册 → 建单 → 提交 → 报表 → access token 失效后自动刷新', async ({ page }) => {
  await registerTenant(page)
  await expect(page.getByRole('heading', { name: '费用单' })).toBeVisible()

  const firstToken = await page.evaluate(() => localStorage.getItem('expensio.access_token'))
  expect(firstToken).toBeTruthy()

  // 建一张草稿并提交，顺带验证 Element Plus 组件是按需注册后真正可用的。
  await page.getByRole('button', { name: '新建费用单' }).click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await page.getByPlaceholder('例如 128.50').fill('88.50')
  await page.getByPlaceholder('例如 汉莎航空').fill(merchant)
  await page.getByRole('button', { name: '保存草稿' }).click()

  await page.waitForURL(/\/expenses\/[0-9a-f-]+$/)
  await expect(page.getByRole('heading', { name: '费用详情' })).toBeVisible()
  await expect(page.getByText(merchant).first()).toBeVisible()
  await expect(page.getByText('草稿', { exact: true })).toBeVisible()

  await page.getByRole('button', { name: '提交审批' }).click()
  await expect(page.getByText('待审批', { exact: true })).toBeVisible()

  // 报表页会动态加载 ECharts；等读模型追上后必须有 canvas，而不是空白。
  await waitForReportData(page)
  await page.goto('/reports')
  await expect(page.getByRole('heading', { name: '报表' })).toBeVisible()
  await expect(page.locator('.chart canvas')).toBeVisible({ timeout: 20_000 })

  // 模拟 access token 过期：保留 refresh token，页面重载后应先 401 再自动刷新。
  await page.goto('/expenses')
  await expect(page.getByText(merchant).first()).toBeVisible()
  await page.evaluate(() => localStorage.setItem('expensio.access_token', 'invalid.token.value'))
  await page.reload()

  await expect(page).toHaveURL(/\/expenses$/)
  await expect(page.getByText(merchant).first()).toBeVisible({ timeout: 20_000 })
  const refreshedToken = await page.evaluate(() => localStorage.getItem('expensio.access_token'))
  expect(refreshedToken).toBeTruthy()
  expect(refreshedToken).not.toBe('invalid.token.value')
})
