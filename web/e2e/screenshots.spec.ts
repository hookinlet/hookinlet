import { createHmac } from 'node:crypto'
import { mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test } from '@playwright/test'
import { ADMIN_TOKEN } from '../playwright.config'
import { githubSignature, signIn } from './helpers'

// Regenerates the console screenshots in docs/images from a seeded server:
//   SCREENSHOTS=1 npm run e2e -- e2e/screenshots.spec.ts
test.skip(!process.env.SCREENSHOTS, 'only when SCREENSHOTS=1')

const OUT = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'docs', 'images')
const admin = { Authorization: `Bearer ${ADMIN_TOKEN}`, 'Content-Type': 'application/json' }

const stripeSig = (secret: string, body: string) => {
  const t = Math.floor(Date.now() / 1000)
  return `t=${t},v1=${createHmac('sha256', secret).update(`${t}.${body}`).digest('hex')}`
}

test('seed an installation and take the screenshots', async ({ page, request }) => {
  mkdirSync(OUT, { recursive: true })
  const make = async (slug: string, verifier: string, secret: string | undefined, extra: object = {}) => {
    const res = await request.post('/v1/admin/inboxes', { headers: admin, data: { slug, verifier, ...(secret ? { verifier_secret: secret } : {}), ...extra } })
    expect(res.ok()).toBeTruthy()
    return (await res.json()).inbox.ingest_url as string
  }
  const stripe = await make('stripe-prod', 'stripe', 'whsec_demo', { name: 'Stripe (production)', retention_days: 30 })
  const github = await make('github-demo', 'github', 'ghsecret', { name: 'GitHub', retention_max_count: 50000 })
  await make('slack-support', 'slack', 'slacksecret', { name: 'Slack support' })
  await make('billing-sandbox', 'hmac', undefined, { name: 'Billing sandbox', verifier_config: { header: 'X-Signature' } })

  const events = ['invoice.paid', 'charge.succeeded', 'customer.updated', 'invoice.created', 'payment_intent.succeeded', 'charge.refunded']
  for (let i = 0; i < 30; i++) {
    const type = events[i % events.length]!
    const body = JSON.stringify({
      id: `evt_1Pq${1000 + i}`,
      type,
      created: 1791798067 + i * 60,
      data: { object: { id: `in_1Pq${2000 + i}`, amount_paid: 1900 + (i % 5) * 1000, currency: 'usd', customer_email: 'ada@example.com', paid: true, lines: { total_count: 1 } } },
    })
    await request.post(stripe, { data: body, headers: { 'Content-Type': 'application/json', 'Stripe-Signature': stripeSig('whsec_demo', body) } })
  }
  for (let i = 0; i < 12; i++) {
    const body = JSON.stringify({ action: i % 3 === 0 ? 'opened' : 'synchronize', number: 40 + i, pull_request: { title: 'Add retries to the relay' }, repository: { full_name: 'acme/api' } })
    await request.post(github, {
      data: body,
      headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': githubSignature('ghsecret', body), 'X-GitHub-Event': 'pull_request', 'X-GitHub-Delivery': `d-${i}` },
    })
  }

  const shot = async (name: string) => page.screenshot({ path: join(OUT, name), animations: 'disabled' })

  await page.emulateMedia({ colorScheme: 'dark' })
  await signIn(page)
  await expect(page.getByRole('link', { name: 'stripe-prod' })).toBeVisible()
  await page.waitForTimeout(1200) // let the activity lines draw
  await shot('console-overview.png')

  await page.getByRole('link', { name: 'stripe-prod' }).click()
  await page.getByRole('button', { name: /#30 / }).click()
  await expect(page.getByText('verified · stripe')).toBeVisible()
  await shot('console-messages.png')

  await page.emulateMedia({ colorScheme: 'light' })
  await page.getByRole('link', { name: 'Settings' }).click()
  await expect(page.getByRole('heading', { name: 'General' })).toBeVisible()
  await shot('console-settings.png')
})
