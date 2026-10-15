import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Locator, type Page } from '@playwright/test'
import { deliver, makeInbox, signIn } from './helpers'

// Every screen, and the states that have their own layout (the created-inbox panel, the open
// palette, a sender's own fields), in both themes and on a phone, has no serious or critical
// accessibility violation that axe can find. axe cannot judge everything, so this is a floor.

async function scan(page: Page, label: string) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze()
  const bad = results.violations.filter((v) => v.impact === 'serious' || v.impact === 'critical')
  expect(bad.map((v) => `${label}: ${v.id} (${v.nodes.length}): ${v.help}: ${v.nodes.map((n) => `${n.target.join(' ')} ${n.any[0]?.message ?? ''}`).join(' | ')}`)).toEqual([])
}

// goTo moves inside the app (a reload would sign out) and waits for what is specific to the page,
// so the scan never runs on a blank or an error page.
async function goTo(page: Page, path: string, ready: Locator) {
  await page.evaluate((p) => {
    window.history.pushState({}, '', p)
    window.dispatchEvent(new PopStateEvent('popstate'))
  }, path)
  await expect(ready).toBeVisible()
}

test('every screen passes axe in the dark and light themes, and on a phone', async ({ page, request }) => {
  const { ingestUrl } = await makeInbox(request, 'a11y-demo')
  await makeInbox(request, 'a11y-open', { verifier: 'none', verifier_secret: undefined }) // checks nothing: the warning states
  await deliver(request, ingestUrl, { a: 1, note: 'hello' }, 'ping')
  await signIn(page, '/admin/')

  const screens: [string, Locator][] = [
    ['/admin/', page.getByRole('heading', { name: 'Overview' })],
    ['/admin/inboxes', page.getByRole('link', { name: 'a11y-demo' })],
    ['/admin/new', page.getByRole('heading', { name: 'New inbox' })],
    ['/admin/tokens', page.getByRole('heading', { name: 'Tokens' })],
    ['/admin/maintenance', page.getByText('Database schema')],
    ['/admin/inboxes/a11y-demo', page.getByRole('button', { name: /#1 ping/ })],
    ['/admin/inboxes/a11y-demo/messages/1', page.getByText('Everything below was written by the sender')],
    ['/admin/inboxes/a11y-demo/settings', page.getByRole('heading', { name: 'General' })],
    ['/admin/inboxes/a11y-open', page.getByRole('note').filter({ hasText: 'Verification is off' })],
    ['/admin/inboxes/a11y-open/settings', page.getByRole('note').filter({ hasText: 'Verification is off' })],
    ['/admin/nothing-here', page.getByRole('heading', { name: 'Not found' })],
  ]

  for (const scheme of ['dark', 'light'] as const) {
    await page.emulateMedia({ colorScheme: scheme })
    for (const [path, ready] of screens) {
      await goTo(page, path, ready)
      await scan(page, `${scheme} ${path}`)
    }

    // a sender with fields of its own
    await goTo(page, '/admin/new', page.getByRole('heading', { name: 'New inbox' }))
    await page.getByText('HMAC', { exact: true }).click()
    await expect(page.getByLabel('Signature header')).toBeVisible()
    await scan(page, `${scheme} new inbox, HMAC fields`)

    // the palette
    await page.keyboard.press('Control+k')
    await expect(page.getByRole('dialog', { name: 'Command palette' })).toBeVisible()
    await scan(page, `${scheme} command palette`)
    await page.keyboard.press('Escape')
  }

  // the created-inbox panel
  await goTo(page, '/admin/new', page.getByRole('heading', { name: 'New inbox' }))
  await page.getByLabel('Slug').fill('a11y-created')
  await page.getByText('None', { exact: true }).click()
  await page.getByRole('button', { name: 'Create inbox' }).click()
  await expect(page.getByText('Created: a11y-created')).toBeVisible()
  await scan(page, 'created inbox panel')

  // a phone
  await page.setViewportSize({ width: 375, height: 812 })
  for (const [path, ready] of screens.slice(0, 8)) {
    await goTo(page, path, ready)
    await scan(page, `phone ${path}`)
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
    expect(overflow, `phone ${path} scrolls sideways`).toBeLessThanOrEqual(0)
  }
  await page.evaluate(() => window.scrollTo(0, 0))
})
