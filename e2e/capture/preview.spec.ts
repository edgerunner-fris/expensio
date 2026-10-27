// 生成 README / 文档用的界面预览图。不是测试用例：它注册真实租户、造数据、
// 再逐页截屏，因此放在独立的 testDir，CI 的 `npx playwright test` 不会执行。
//
// 用法（整栈起来之后）：
//   docker compose --profile app --profile e2e run --rm \
//     -e PREVIEW_OUT=/out \
//     -v "$PWD/docs/screenshots:/out" \
//     e2e npx playwright test --config playwright.capture.config.ts
//
// 数据全部经真实 API 产生（注册 → 预算 → 费用 → 邀请 admin → 审批），
// 不直接写数据库，也不 mock 任何请求。
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'
import { mkdir } from 'node:fs/promises'
import { join } from 'node:path'
import { registerTenant, type TenantAccount } from '../tests/support'

const outDir = process.env.PREVIEW_OUT ?? 'screenshots'

const runId = Date.now().toString(36)
const owner: TenantAccount = {
  slug: `demo-${runId}`,
  name: 'Acme 科技',
  email: `owner-demo-${runId}@example.com`,
  password: 'demo-owner-pass',
  displayName: '李明',
}
const adminEmail = `admin-demo-${runId}@example.com`
const adminPassword = 'demo-admin-pass'

interface Session {
  access_token: string
}

interface Expense {
  id: string
  status: string
}

interface CreatedInvitation {
  link: string
}

function bearer(token: string): Record<string, string> {
  return { Authorization: `Bearer ${token}` }
}

async function post<T>(
  request: APIRequestContext,
  token: string | null,
  url: string,
  data?: unknown,
): Promise<T> {
  const resp = await request.post(url, {
    headers: token ? bearer(token) : {},
    data: data ?? {},
  })
  const text = await resp.text()
  expect(resp.ok(), `POST ${url} -> ${resp.status()} ${text}`).toBeTruthy()
  return (JSON.parse(text) as { data: T }).data
}

async function shot(page: Page, name: string): Promise<void> {
  // 等 Element Plus 的过渡动画收尾，避免截到半透明的遮罩。
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(outDir, name), fullPage: true })
}

async function accessToken(page: Page): Promise<string> {
  const token = await page.evaluate(() => localStorage.getItem('expensio.access_token'))
  expect(token).toBeTruthy()
  return token as string
}

test('生成界面预览图', async ({ page }) => {
  await mkdir(outDir, { recursive: true })

  await page.goto('/login')
  await expect(page.getByRole('heading', { name: '登录' })).toBeVisible()
  await shot(page, '01-login.png')

  await registerTenant(page, owner)
  const token = await accessToken(page)

  const today = new Date()
  const iso = (d: Date): string => d.toISOString().slice(0, 10)
  const periodStart = iso(new Date(today.getFullYear(), today.getMonth(), 1))
  const periodEnd = iso(new Date(today.getFullYear(), today.getMonth() + 1, 0))

  // 规则：租户一旦有预算，没有匹配 (类别, 币种, 日期) 预算的费用就不能提交。
  // 所以演示数据的每个类别都建一条预算，而不是只建差旅。
  const budgetSpecs = [
    { name: '差旅预算', category: 'travel', currency: 'CNY', amount_minor: 2_000_000 },
    { name: '餐饮预算', category: 'meals', currency: 'CNY', amount_minor: 300_000 },
    { name: '软件订阅', category: 'software', currency: 'USD', amount_minor: 100_000 },
    { name: '办公用品', category: 'office', currency: 'CNY', amount_minor: 500_000 },
    { name: '其他支出', category: 'other', currency: 'CNY', amount_minor: 200_000 },
  ]
  const budgets = new Map<string, string>()
  for (const spec of budgetSpecs) {
    const created = await post<{ id: string }>(page.request, token, '/api/v1/budgets', {
      ...spec,
      period_start: periodStart,
      period_end: periodEnd,
    })
    budgets.set(`${spec.category}/${spec.currency}`, created.id)
  }

  const drafts = [
    {
      category: 'travel',
      currency: 'CNY',
      amount_minor: 328_000,
      merchant: '中国国航',
      description: '上海 → 北京 往返机票',
      occurred_at: today.toISOString(),
    },
    {
      category: 'travel',
      currency: 'CNY',
      amount_minor: 96_000,
      merchant: '华住酒店',
      description: '两晚住宿（出差）',
      occurred_at: today.toISOString(),
    },
    {
      category: 'meals',
      currency: 'CNY',
      amount_minor: 26_800,
      merchant: '客户工作餐',
      description: '与客户团队商务午餐',
      occurred_at: today.toISOString(),
    },
    {
      category: 'software',
      currency: 'USD',
      amount_minor: 4_900,
      merchant: 'JetBrains',
      description: '团队 IDE 年度订阅',
      occurred_at: today.toISOString(),
    },
    {
      category: 'office',
      currency: 'CNY',
      amount_minor: 15_900,
      merchant: '京东办公',
      description: '显示器支架与键盘',
      occurred_at: today.toISOString(),
    },
    {
      category: 'other',
      currency: 'CNY',
      amount_minor: 8_800,
      merchant: '顺丰速运',
      description: '合同文件寄送',
      occurred_at: today.toISOString(),
    },
  ]

  const expenses: Expense[] = []
  for (const draft of drafts) {
    const created = await post<Expense>(page.request, token, '/api/v1/expenses', draft)
    // 最后一条保留草稿状态，让列表同时展示完整的状态分布。
    if (expenses.length < drafts.length - 1) {
      await post<Expense>(page.request, token, `/api/v1/expenses/${created.id}/submit`)
    }
    expenses.push(created)
  }

  // 自审批被禁止，所以邀请一个 admin 来审批：链接与接受都走真实接口。
  const invitation = await post<CreatedInvitation>(
    page.request,
    token,
    '/api/v1/tenants/current/invitations',
    { email: adminEmail, role: 'admin' },
  )
  const inviteToken = new URL(invitation.link).pathname.split('/').filter(Boolean).pop()
  expect(inviteToken, '邀请链接应包含一次性令牌').toBeTruthy()
  const adminSession = await post<Session>(
    page.request,
    null,
    `/api/v1/invitations/${encodeURIComponent(inviteToken ?? '')}/accept`,
    { password: adminPassword, display_name: '王芳' },
  )

  await post(page.request, adminSession.access_token, `/api/v1/expenses/${expenses[0].id}/approve`, {
    comment: '同意',
  })
  await post(page.request, adminSession.access_token, `/api/v1/expenses/${expenses[1].id}/approve`, {
    comment: '同意',
  })
  await post(page.request, adminSession.access_token, `/api/v1/expenses/${expenses[2].id}/approve`, {
    comment: '同意',
  })
  await post(page.request, adminSession.access_token, `/api/v1/expenses/${expenses[3].id}/reject`, {
    comment: '请补充采购审批单',
  })

  // 报表是事件驱动的最终一致读模型：等消费者追上再截图。
  await expect
    .poll(
      async () => {
        const from = iso(new Date(today.getFullYear(), today.getMonth() - 1, 1))
        const resp = await page.request.get(
          `/api/v1/reports/expenses/summary?from=${from}&to=${iso(today)}&group_by=category&scope=all`,
          { headers: bearer(token) },
        )
        if (!resp.ok()) return 0
        const body = (await resp.json()) as {
          data?: { currency_totals?: unknown[] }
        }
        return body.data?.currency_totals?.length ?? 0
      },
      { timeout: 30_000, message: '等待报表读模型消费费用事件' },
    )
    .toBeGreaterThan(0)

  await page.goto('/expenses')
  await expect(page.getByRole('heading', { name: '费用单' })).toBeVisible()
  await expect(page.locator('.el-table__row').first()).toBeVisible()
  await shot(page, '02-expenses.png')

  await page.goto(`/expenses/${expenses[0].id}`)
  await expect(page.getByRole('heading', { name: '费用详情' })).toBeVisible()
  await shot(page, '03-expense-detail.png')

  await page.goto('/reports')
  await expect(page.getByRole('heading', { name: '报表' })).toBeVisible()
  await expect(page.locator('.chart canvas')).toBeVisible()
  await shot(page, '04-reports.png')

  await page.goto('/budgets')
  await expect(page.getByRole('heading', { name: '预算' })).toBeVisible()
  await expect(page.getByText('差旅预算').first()).toBeVisible()
  await shot(page, '05-budgets.png')

  await page.goto('/members')
  await expect(page.getByRole('heading', { name: '成员' })).toBeVisible()
  await expect(page.getByText(adminEmail).first()).toBeVisible()
  await shot(page, '06-members.png')

  await page.goto('/billing')
  await expect(page.getByRole('heading', { name: '账单与订阅', exact: true })).toBeVisible()
  await shot(page, '07-billing.png')

  await page.goto('/notifications')
  await expect(page.getByRole('heading', { name: '通知' })).toBeVisible()
  await shot(page, '08-notifications.png')
})
