import { defineConfig, devices } from '@playwright/test'

// `make e2e` starts docker-manager on :8091 with a fresh database against the local Docker and
// dev/apps, then runs these tests.
export default defineConfig({
  testDir: 'e2e',
  timeout: 60_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  reporter: [['list']],
  use: {
    baseURL: process.env.DM_E2E_URL ?? 'http://localhost:8091',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'], channel: undefined } }],
})
