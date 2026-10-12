import { createHmac } from 'node:crypto'
import { expect, type APIRequestContext, type Page } from '@playwright/test'
import { ADMIN_TOKEN } from '../playwright.config'

export const SECRET = 'e2e-webhook-secret'
const adminHeaders = { Authorization: `Bearer ${ADMIN_TOKEN}`, 'Content-Type': 'application/json' }

export async function signIn(page: Page, path = '/admin/') {
  await page.goto(path)
  await page.getByLabel('Admin token').fill(ADMIN_TOKEN)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible()
}

// A GitHub-style signature for a body, so a delivery verifies.
export function githubSignature(secret: string, body: string): string {
  return 'sha256=' + createHmac('sha256', secret).update(body).digest('hex')
}

// makeInbox creates a GitHub-verified inbox through the admin API, so a test starts from its own
// state and does not depend on another test having run.
export async function makeInbox(request: APIRequestContext, slug: string, extra: object = {}) {
  const res = await request.post('/v1/admin/inboxes', { headers: adminHeaders, data: { slug, verifier: 'github', verifier_secret: SECRET, ...extra } })
  expect(res.ok()).toBeTruthy()
  const made = await res.json()
  return { ingestUrl: made.inbox.ingest_url as string, agentToken: made.agent_token as string }
}

// deliver posts a correctly signed GitHub delivery to an inbox.
export async function deliver(request: APIRequestContext, ingestUrl: string, body: object, event = 'push', id = `d-${Math.random().toString(36).slice(2)}`) {
  const text = JSON.stringify(body)
  return request.post(ingestUrl, {
    data: text,
    headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': githubSignature(SECRET, text), 'X-GitHub-Event': event, 'X-GitHub-Delivery': id },
  })
}
