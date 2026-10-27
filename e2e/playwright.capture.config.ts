import { defineConfig, devices } from '@playwright/test'

// 截图不是测试：独立 config 与 testDir，默认 `npx playwright test` 不会跑到它。
// 用法见 e2e/capture/preview.spec.ts 顶部注释。
const baseURL = process.env.E2E_BASE_URL ?? 'http://127.0.0.1:5173'

export default defineConfig({
  testDir: './capture',
  timeout: 180_000,
  expect: { timeout: 15_000 },
  workers: 1,
  reporter: [['list']],
  use: {
    baseURL,
    locale: 'zh-CN',
    // 固定视口，保证每次生成的预览图尺寸一致；截图本身不分页。
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: 1,
    trace: 'off',
    screenshot: 'off',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1 },
    },
  ],
})
