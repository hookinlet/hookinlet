import { createHmac } from 'node:crypto'
import { expect, type Page } from '@playwright/test'
import { ADMIN_TOKEN } from '../playwright.config'

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
