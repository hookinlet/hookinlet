import { ref } from 'vue'
import { ApiError, request } from './api/client'

// The admin token is held here, in memory, and nowhere else: not in a URL, not in
// storage. Reloading the page signs out.
const token = ref('')
export const signedIn = ref(false)

// Whatever holds data read with the token registers here, so that signing out (by the
// person, or because the server said 401) drops all of it.
const onSignOut: Array<() => void> = []
export function whenSignedOut(fn: () => void): void {
  onSignOut.push(fn)
}

export function currentToken(): string {
  return token.value
}

export async function signIn(candidate: string): Promise<void> {
  // A 401 says the token is wrong, a 403 that it cannot use the admin API. Listing
  // inboxes is an operation every admin may call, including a tenant's admin, which
  // the store-wide ones (status, tokens, backups) refuse.
  await request(candidate, '/v1/admin/inboxes?limit=1')
  token.value = candidate
  signedIn.value = true
}

export function signOut(): void {
  token.value = ''
  signedIn.value = false
  for (const fn of onSignOut) fn()
}

// guard runs an API call with the session's token. A 401 means the token was revoked or
// the server was restarted with another one, so the session ends and the sign-in page
// comes back.
export async function guard<T>(call: (token: string) => Promise<T>): Promise<T> {
  try {
    return await call(token.value)
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) signOut()
    throw e
  }
}
