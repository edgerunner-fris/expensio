import { defineConfig, devices } from '@playwright/test'

// 默认打完整容器栈的 web（make app-up 之后）。也可以指向本地 vite dev：
//   E2E_BASE_URL=http://127.0.0.1:5173 npx playwright test
const baseURL = process.env.E2E_BASE_URL ?? 'http://127.0.0.1:5173'

export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  // 用例会注册真实租户并写真实数据库；串行执行，避免相互干扰。
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list']],
  use: {
    baseURL,
    locale: 'zh-CN',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
