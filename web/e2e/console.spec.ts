import { expect, test } from '@playwright/test'
import { githubSignature, signIn } from './helpers'

const SECRET = 'e2e-webhook-secret'
let ingestUrl = ''
let agentToken = ''

test.describe.configure({ mode: 'serial' })

test('the wrong token is refused and the right one signs in', async ({ page }) => {
  await page.goto('/admin/')
  await page.getByLabel('Admin token').fill('not-the-token')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('alert')).toContainText('not this server’s admin token')
  await signIn(page)
  await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible()
})

test('create an inbox, receive a signed delivery, and read it', async ({ page, request }) => {
  await signIn(page, '/admin/new')
  await page.getByLabel('Slug').fill('github-e2e')
  await page.getByText('GitHub', { exact: true }).click()
  await page.getByLabel('Webhook secret').fill(SECRET)
  await page.getByRole('button', { name: 'Create inbox' }).click()

  await expect(page.getByText('Created: github-e2e')).toBeVisible()
  ingestUrl = await page.getByLabel('Ingest URL').innerText()
  agentToken = await page.getByLabel('Agent token').innerText()
  expect(ingestUrl).toMatch(/\/in\/[A-Za-z0-9_-]{16,}$/)
  expect(agentToken).toMatch(/^hki_/)

  const body = JSON.stringify({ action: 'opened', note: '<img src=x onerror=alert(1)>' })
  const res = await request.post(ingestUrl, {
    data: body,
    headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': githubSignature(SECRET, body), 'X-GitHub-Event': 'pull_request', 'X-GitHub-Delivery': 'e2e-1' },
  })
  expect(res.status()).toBe(200)

  await page.getByRole('link', { name: 'Open the inbox' }).click()
  await page.getByRole('button', { name: /#1 pull_request/ }).click()
  await expect(page.getByText('verified · github')).toBeVisible()
  // Sender content is text: the markup is shown, not interpreted.
  await expect(page.getByText('<img src=x onerror=alert(1)>')).toBeVisible()
  await page.getByRole('tab', { name: 'Raw' }).click()
  await expect(page.getByText('bytes, exactly as received')).toBeVisible()
  await page.getByRole('tab', { name: /^Headers/ }).click()
  await expect(page.getByRole('cell', { name: 'application/json' }).first()).toBeVisible()
})

test('live mode shows a delivery as it arrives', async ({ page, request }) => {
  await signIn(page, '/admin/inboxes/github-e2e')
  await page.getByRole('button', { name: 'Go live' }).click()
  await expect(page.getByRole('button', { name: 'Live' })).toBeVisible()
  const body = '{"n":2}'
  await request.post(ingestUrl, {
    data: body,
    headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': githubSignature(SECRET, body), 'X-GitHub-Event': 'push', 'X-GitHub-Delivery': 'e2e-2' },
  })
  await expect(page.getByRole('button', { name: /#2 push/ })).toBeVisible()
})

test('a forged delivery is refused and nothing is stored', async ({ request }) => {
  const res = await request.post(ingestUrl, { data: '{"n":3}', headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': 'sha256=00' } })
  expect(res.status()).toBe(401)
})

test('issue a token that reads the inbox, then revoke it', async ({ page, request }) => {
  await signIn(page, '/admin/tokens')
  await page.getByRole('button', { name: 'New token' }).click()
  await page.getByLabel('Name').fill('e2e reader')
  await page.getByRole('button', { name: 'Create token' }).click()
  const token = await page.getByLabel('The new token').innerText()
  const read = await request.get('/v1/inboxes/github-e2e/messages?after=0', { headers: { Authorization: `Bearer ${token}` } })
  expect(read.status()).toBe(200)
  await page.getByRole('button', { name: 'I have stored it' }).click()

  const row = page.getByRole('row', { name: /e2e reader/ })
  await row.getByRole('button', { name: 'Revoke' }).click()
  await row.getByRole('button', { name: 'Click to confirm' }).click()
  await expect(page.getByRole('row', { name: /e2e reader/ })).toHaveCount(0)
  const after = await request.get('/v1/inboxes/github-e2e/messages?after=0', { headers: { Authorization: `Bearer ${token}` } })
  expect(after.status()).toBe(401)
})

test('settings: rename, rotate the ingest URL, and delete with a typed confirmation', async ({ page, request }) => {
  await signIn(page, '/admin/inboxes/github-e2e/settings')
  await page.getByLabel('Name', { exact: true }).fill('GitHub (e2e)')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Saved.' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'GitHub (e2e)' })).toBeVisible()

  await page.getByRole('button', { name: 'Rotate the ingest URL' }).click()
  await page.getByRole('button', { name: 'Click again to rotate' }).click()
  await expect(page.getByText('A new URL is in use')).toBeVisible()
  const old = await request.post(ingestUrl, { data: '{}', headers: { 'Content-Type': 'application/json' } })
  expect(old.status()).toBe(404)

  const del = page.getByRole('button', { name: 'Delete inbox' })
  await expect(del).toBeDisabled()
  await page.getByLabel('Inbox slug').fill('github-e2e')
  await del.click()
  await expect(page).toHaveURL(/\/admin\/inboxes$/)
  await expect(page.getByRole('heading', { name: 'Inboxes' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'github-e2e' })).toHaveCount(0)
})

test('maintenance: run retention and download a backup', async ({ page }) => {
  await signIn(page, '/admin/maintenance')
  await expect(page.getByText('Database schema')).toBeVisible()
  await page.getByRole('button', { name: 'Run retention now' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Done.' })).toBeVisible()
  const download = page.waitForEvent('download')
  await page.getByRole('button', { name: 'Download a backup' }).click()
  expect((await download).suggestedFilename()).toMatch(/^hookinlet-backup-.*\.db$/)
})

test('the command palette finds a screen and goes there', async ({ page }) => {
  await signIn(page)
  await page.keyboard.press('Control+k')
  await expect(page.getByRole('dialog', { name: 'Command palette' })).toBeVisible()
  await page.getByLabel('Search commands').fill('maint')
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(/\/admin\/maintenance$/)
  await expect(page.getByRole('heading', { name: 'Maintenance' })).toBeVisible()
})

test('a reload signs out, because the token is only in memory', async ({ page }) => {
  await signIn(page, '/admin/tokens')
  await page.reload()
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).not.toContain('e2e-admin-token')
})
