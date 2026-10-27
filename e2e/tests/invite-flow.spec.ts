import { expect, test, type APIRequestContext } from '@playwright/test'
import { invitePath, registerTenant, uniqueAccount } from './support'

const owner = uniqueAccount('e2e-inv')
const inviteeEmail = `invitee-${owner.slug}@example.com`
const inviteePassword = `invitee-${owner.password}`
const inviteeName = 'E2E 受邀者'

// compose 的 e2e 服务会注入 http://mailpit:8025；本地跑 vite dev 时可以不配，
// 此时只跳过"邮件是否真正送达"这一段，邀请本身仍走完整 UI 流程。
const mailpitURL = process.env.E2E_MAILPIT_URL ?? ''

interface MailpitMessage {
  ID: string
  Subject: string
  To?: { Address?: string }[]
}

async function waitForInviteMail(request: APIRequestContext): Promise<string> {
  let messageID = ''
  await expect
    .poll(
      async () => {
        const resp = await request.get(`${mailpitURL}/api/v1/messages?limit=200`)
        if (!resp.ok()) return ''
        const body = (await resp.json()) as { messages?: MailpitMessage[] }
        const found = body.messages?.find((m) =>
          m.To?.some((to) => to.Address?.toLowerCase() === inviteeEmail),
        )
        messageID = found?.ID ?? ''
        return messageID
      },
      { timeout: 30_000, message: '等待邀请邮件进入 Mailpit' },
    )
    .not.toBe('')
  return messageID
}

test('邀请成员 → 邮件送达 → 公开页接受 → 新成员可登录', async ({ page, request }) => {
  await registerTenant(page, owner)

  // 成员页：创建邀请并拿到一次性链接。
  await page.goto('/members')
  await expect(page.getByRole('heading', { name: '成员' })).toBeVisible()
  await page.getByRole('button', { name: '邀请成员' }).click()
  const inviteDialog = page.getByRole('dialog', { name: '邀请成员' })
  await inviteDialog.getByPlaceholder('teammate@company.com').fill(inviteeEmail)
  await inviteDialog.getByRole('button', { name: '发送邀请' }).click()

  const resultDialog = page.getByRole('dialog', { name: '邀请已创建' })
  await expect(resultDialog).toBeVisible()
  await expect(resultDialog.getByText('邀请邮件已排队发送')).toBeVisible()
  const link = await resultDialog.locator('input').inputValue()
  expect(link).toContain('/invite/')
  const path = invitePath(link)
  await resultDialog.getByRole('button', { name: '完成' }).click()

  // 待接受列表里能看到这条邀请，且只有一次待接受记录。
  await expect(page.getByText(inviteeEmail).first()).toBeVisible()

  // 邮件正文必须带同一个一次性链接，证明 notification outbox → SMTP 真正投递。
  if (mailpitURL) {
    const messageID = await waitForInviteMail(request)
    const detailResp = await request.get(`${mailpitURL}/api/v1/message/${messageID}`, {
      headers: { Accept: 'application/json' },
    })
    expect(detailResp.ok()).toBeTruthy()
    const detail = (await detailResp.json()) as { Text?: string; HTML?: string }
    expect(`${detail.Text ?? ''}\n${detail.HTML ?? ''}`).toContain(path)
  }

  // 公开接受页：新邮箱需要设置密码和姓名。
  await page.goto(path)
  await expect(page.getByRole('heading', { name: `加入 ${owner.name}` })).toBeVisible()
  await page.getByPlaceholder('例如 李明').fill(inviteeName)
  await page.locator('input[type="password"]').fill(inviteePassword)
  await page.getByRole('button', { name: '设置密码并加入' }).click()
  await page.waitForURL('**/expenses')
  await expect(page.getByRole('heading', { name: '费用单' })).toBeVisible()

  // 会话已切到被邀请租户，并且只拿到 member 角色。
  await expect(page.locator('.tenant-name')).toHaveText(owner.name)
  await expect(page.locator('.tenant')).toContainText('成员')

  // 链接是一次性的：再次打开必须显示已使用。
  await page.goto(path)
  await expect(page.getByRole('heading', { name: '邀请不可用' })).toBeVisible()
  await expect(page.getByText('邀请已被使用')).toBeVisible()

  // 退出后用自己的邮箱、密码和租户标识登录，证明接受时设置的密码真实可用。
  await page.goto('/expenses')
  await page.locator('.user-trigger').click()
  await page.getByRole('menuitem', { name: '退出登录' }).click()
  await page.waitForURL(/\/login/)
  await page.getByPlaceholder('例如 acme', { exact: true }).fill(owner.slug)
  await page.getByPlaceholder('you@company.com').fill(inviteeEmail)
  await page.locator('input[type="password"]').fill(inviteePassword)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await page.waitForURL('**/expenses')
  await expect(page.getByRole('heading', { name: '费用单' })).toBeVisible()

  // member 角色无权读取成员管理接口，换回 owner 确认新成员已经落库。
  await page.locator('.user-trigger').click()
  await page.getByRole('menuitem', { name: '退出登录' }).click()
  await page.waitForURL(/\/login/)
  await page.getByPlaceholder('例如 acme', { exact: true }).fill(owner.slug)
  await page.getByPlaceholder('you@company.com').fill(owner.email)
  await page.locator('input[type="password"]').fill(owner.password)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await page.waitForURL('**/expenses')
  await page.goto('/members')
  await expect(page.getByText(inviteeEmail).first()).toBeVisible()
  await expect(page.getByText(owner.email).first()).toBeVisible()
})
