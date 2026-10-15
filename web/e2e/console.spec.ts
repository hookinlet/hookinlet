import { expect, test } from '@playwright/test'
import { ADMIN_TOKEN } from '../playwright.config'
import { deliver, githubSignature, makeInbox, SECRET, signIn } from './helpers'

// Each test makes the inbox it needs through the API, so any one can run alone.

test('the wrong token is refused and the right one signs in', async ({ page }) => {
  await page.goto('/admin/')
  await page.getByLabel('Admin token').fill('not-the-token')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('alert')).toContainText('not this server’s admin token')
  await signIn(page)
  await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible()
})

test('create an inbox in the console, receive a signed delivery, and read it', async ({ page, request }) => {
  await signIn(page, '/admin/new')
  await page.getByLabel('Slug').fill('github-create')
  await page.getByText('GitHub', { exact: true }).click()
  await page.getByLabel('Webhook secret').fill(SECRET)
  await page.getByRole('button', { name: 'Create inbox' }).click()

  await expect(page.getByText('Created: github-create')).toBeVisible()
  const ingestUrl = await page.getByLabel('Ingest URL').innerText()
  const agentToken = await page.getByLabel('Agent token').innerText()
  expect(ingestUrl).toMatch(/\/in\/[A-Za-z0-9_-]{16,}$/)
  expect(agentToken).toMatch(/^hki_/)

  const res = await deliver(request, ingestUrl, { action: 'opened', note: '<img src=x onerror=alert(1)>' }, 'pull_request', 'e2e-1')
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
  const { ingestUrl } = await makeInbox(request, 'github-live')
  await signIn(page, '/admin/inboxes/github-live')
  await page.getByRole('button', { name: 'Go live' }).click()
  await expect(page.getByRole('button', { name: 'Live' })).toBeVisible()
  await deliver(request, ingestUrl, { n: 1 }, 'push')
  await expect(page.getByRole('button', { name: /#1 push/ })).toBeVisible()
})

test('a forged delivery is refused and nothing is stored', async ({ request }) => {
  const { ingestUrl, agentToken } = await makeInbox(request, 'github-forged')
  const res = await request.post(ingestUrl, { data: '{"n":3}', headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': 'sha256=00' } })
  expect(res.status()).toBe(401)
  const read = await request.get('/v1/inboxes/github-forged/messages?after=0', { headers: { Authorization: `Bearer ${agentToken}` } })
  expect((await read.json()).messages).toHaveLength(0)
})

test('issue a token that reads the inbox, then revoke it', async ({ page, request }) => {
  await makeInbox(request, 'github-token')
  await signIn(page, '/admin/tokens')
  await page.getByRole('button', { name: 'New token' }).click()
  await page.getByLabel('Name').fill('e2e reader')
  await page.getByRole('button', { name: 'Create token' }).click()
  const token = await page.getByLabel('The new token').innerText()
  const read = await request.get('/v1/inboxes/github-token/messages?after=0', { headers: { Authorization: `Bearer ${token}` } })
  expect(read.status()).toBe(200)
  await page.getByRole('button', { name: 'I have stored it' }).click()

  const row = page.getByRole('row', { name: /e2e reader/ })
  await row.getByRole('button', { name: 'Revoke' }).click()
  await row.getByRole('button', { name: 'Click to confirm' }).click()
  await expect(page.getByRole('row', { name: /e2e reader/ })).toHaveCount(0)
  const after = await request.get('/v1/inboxes/github-token/messages?after=0', { headers: { Authorization: `Bearer ${token}` } })
  expect(after.status()).toBe(401)
})

test('settings: a save sends only what changed, and keeps what the form does not show', async ({ page, request }) => {
  // an HMAC inbox with a setting the form has no field for, disabled by someone else
  const res = await request.post('/v1/admin/inboxes', {
    headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, 'Content-Type': 'application/json' },
    data: { slug: 'hmac-keep', verifier: 'hmac', verifier_secret: 's', verifier_config: { header: 'X-Sig', timestamp_header: 'X-Ts', payload_template: 'v0:{timestamp}:{body}' } },
  })
  expect(res.ok()).toBeTruthy()
  await signIn(page, '/admin/inboxes/hmac-keep/settings')
  await expect(page.getByRole('heading', { name: 'General' })).toBeVisible()
  // someone else disables it while the page is open
  await request.patch('/v1/admin/inboxes/hmac-keep', {
    headers: { Authorization: `Bearer ${ADMIN_TOKEN}`, 'Content-Type': 'application/json' },
    data: { disabled: true },
  })
  await page.getByLabel('Name', { exact: true }).fill('HMAC kept')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Saved.' })).toBeVisible()

  const show = await request.get('/v1/admin/inboxes/hmac-keep', { headers: { Authorization: `Bearer ${ADMIN_TOKEN}` } })
  const inbox = await show.json()
  expect(inbox.name).toBe('HMAC kept')
  expect(inbox.disabled).toBe(true) // not undone by the save
  expect(inbox.verifier_config.payload_template).toBe('v0:{timestamp}:{body}') // not dropped
})

test('settings: rename, rotate the ingest URL, and delete with a typed confirmation', async ({ page, request }) => {
  const { ingestUrl } = await makeInbox(request, 'github-manage')
  await signIn(page, '/admin/inboxes/github-manage/settings')
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
  await page.getByLabel('Inbox slug').fill('github-manage')
  await del.click()
  await expect(page).toHaveURL(/\/admin\/inboxes$/)
  await expect(page.getByRole('heading', { name: 'Inboxes' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'github-manage' })).toHaveCount(0)
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

test('direction-changing characters in a body, a path and headers are shown as code points', async ({ page, request }) => {
  const { ingestUrl } = await makeInbox(request, 'github-spoof')
  const body = JSON.stringify({ 'k‮ey': 'val‮ue' })
  await request.post(`${ingestUrl}/invoice%E2%80%AEfdp.exe`, {
    data: body,
    headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': githubSignature(SECRET, body), 'X-GitHub-Event': 'push' },
  })
  await signIn(page, '/admin/inboxes/github-spoof/messages/1')
  await expect(page.getByText('<U+202E>').first()).toBeVisible()
  const html = await page.getByRole('main').innerHTML()
  expect(html).not.toContain('‮') // none of the raw characters reached the page
})

test('an inbox that checks nothing says so in the list, on its page, in its settings and when it is made', async ({ page, request }) => {
  await makeInbox(request, 'checked-gh')

  // Made in the console: the form warns while None is chosen, and the created panel says it again.
  await signIn(page, '/admin/new')
  await page.getByLabel('Slug').fill('open-none')
  await expect(page.getByRole('note').filter({ hasText: 'Verification is off' })).toHaveCount(0)
  await page.getByText('None', { exact: true }).click()
  await expect(page.getByRole('note').filter({ hasText: 'Verification is off' })).toBeVisible()
  await page.getByRole('button', { name: 'Create inbox' }).click()
  await expect(page.getByText('Created: open-none')).toBeVisible()
  await expect(page.getByRole('note').filter({ hasText: 'does not verify senders' })).toBeVisible()

  // Its page: a warning pill and a note that links to the settings, where the form warns too.
  await page.getByRole('link', { name: 'Open the inbox' }).click()
  await expect(page.getByText('none (unverified)')).toBeVisible()
  await expect(page.getByRole('note').filter({ hasText: 'Verification is off' })).toBeVisible()
  await page.getByRole('link', { name: 'Choose a verifier' }).click()
  await expect(page.getByRole('heading', { name: 'General' })).toBeVisible()
  await expect(page.getByRole('note').filter({ hasText: 'Verification is off' })).toBeVisible()

  // The list marks that inbox and not the verified one.
  await signIn(page, '/admin/inboxes')
  await expect(page.getByRole('row', { name: /open-none/ })).toContainText('none (unverified)')
  await expect(page.getByRole('row', { name: /checked-gh/ })).not.toContainText('unverified')

  // A verified inbox has no warning on its page.
  await signIn(page, '/admin/inboxes/checked-gh')
  await expect(page.getByRole('heading', { name: 'checked-gh' })).toBeVisible()
  await expect(page.getByRole('note').filter({ hasText: 'Verification is off' })).toHaveCount(0)
})
