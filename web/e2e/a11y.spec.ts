import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { githubSignature, signIn } from './helpers'

// Every screen, in both themes, has no serious or critical accessibility violation that axe can find.
// (axe cannot judge everything, so this is a floor, not a verdict.)
const PAGES = ['/admin/', '/admin/inboxes', '/admin/new', '/admin/tokens', '/admin/maintenance', '/admin/inboxes/a11y-demo', '/admin/inboxes/a11y-demo/messages/1', '/admin/inboxes/a11y-demo/settings']

async function scan(page: Page, label: string) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze()
  const bad = results.violations.filter((v) => v.impact === 'serious' || v.impact === 'critical')
  expect(bad.map((v) => `${label}: ${v.id} (${v.nodes.length}): ${v.help}: ${v.nodes.map((n) => `${n.target.join(' ')} ${n.any[0]?.message ?? ''}`).join(' | ')}`)).toEqual([])
}

test('every screen passes axe in the dark and light themes', async ({ page, request }) => {
  // an inbox with a message to show (signing in on the new-inbox page keeps the token in memory)
  await signIn(page, '/admin/new')
  await page.getByLabel('Slug').fill('a11y-demo')
  await page.getByText('GitHub', { exact: true }).click()
  await page.getByLabel('Webhook secret').fill('s')
  await page.getByRole('button', { name: 'Create inbox' }).click()
  const url = await page.getByLabel('Ingest URL').innerText()
  const body = '{"a":1}'
  await request.post(url, { data: body, headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': githubSignature('s', body), 'X-GitHub-Event': 'ping' } })

  for (const theme of ['dark', 'light'] as const) {
    await page.emulateMedia({ colorScheme: theme })
    for (const path of PAGES) {
      // moving inside the app keeps the token in memory
      await page.evaluate((p) => {
        window.history.pushState({}, '', p)
        window.dispatchEvent(new PopStateEvent('popstate'))
      }, path)
      await page.waitForLoadState('networkidle')
      await page.getByRole('main').waitFor()
      await page.waitForTimeout(300)
      await scan(page, `${theme} ${path}`)
    }
  }
})
