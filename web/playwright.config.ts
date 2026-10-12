import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { defineConfig } from '@playwright/test'

// The end-to-end tests run the real hookinlet binary (built by `npm run e2e`, which builds it
// from the repository) on a throwaway data directory, and drive the console in a browser.
const PORT = 18181
export const ADMIN_TOKEN = 'e2e-admin-token-0123456789-abcdefghij'

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  workers: 1, // one server, one database: the tests build on each other
  timeout: 30_000,
  expect: { timeout: 7_000 },
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: { baseURL: `http://127.0.0.1:${PORT}`, trace: 'retain-on-failure', screenshot: 'only-on-failure' },
  webServer: {
    command: '../bin/hookinlet-e2e serve',
    url: `http://127.0.0.1:${PORT}/readyz`,
    reuseExistingServer: false,
    timeout: 30_000,
    env: {
      HOOKINLET_ADMIN_TOKEN: ADMIN_TOKEN,
      HOOKINLET_MASTER_KEY: 'AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=',
      HOOKINLET_DATA_DIR: mkdtempSync(join(tmpdir(), 'hookinlet-e2e-')),
      HOOKINLET_LISTEN_ADDR: `127.0.0.1:${PORT}`,
      HOOKINLET_LOG_LEVEL: 'warn',
    },
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium', viewport: { width: 1280, height: 800 } } }],
})
