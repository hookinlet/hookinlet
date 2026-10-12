import type { components } from './schema'

export type Status = components['schemas']['AdminStatus']
export type Inbox = components['schemas']['Admin']
export type Token = components['schemas']['Token']
export type TokenCreated = components['schemas']['TokenCreated']
export type MessagePage = components['schemas']['MessagePage']

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }
}

// request calls the API with the admin token. The token lives in the caller's memory
// only; it is never put in a URL.
export async function request<T>(token: string, path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Authorization', `Bearer ${token}`)
  headers.set('Accept', 'application/json')
  if (init.body !== undefined && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  const res = await fetch(path, { ...init, headers })
  if (!res.ok) {
    let message = res.statusText
    try {
      const body = (await res.json()) as { error?: { message?: string } }
      message = body.error?.message ?? message
    } catch {
      // not JSON: keep the status text
    }
    throw new ApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export const getStatus = (token: string) => request<Status>(token, '/v1/admin/status')

// every collects all pages of a list endpoint, up to a safety limit.
async function every<T, K extends string>(token: string, path: string, key: K, max = 20000): Promise<T[]> {
  const out: T[] = []
  let after = 0
  for (;;) {
    const sep = path.includes('?') ? '&' : '?'
    const page = await request<Record<K, T[]> & { next: string | null }>(token, `${path}${sep}limit=500&after=${after}`)
    out.push(...page[key])
    if (!page.next || page[key].length === 0) return out
    if (out.length >= max) throw new Error(`More than ${max} entries; list the rest with the command line.`)
    const m = /[?&]after=(\d+)/.exec(page.next)
    if (!m) return out
    after = Number(m[1])
  }
}

export const listInboxes = (token: string) => every<Inbox, 'inboxes'>(token, '/v1/admin/inboxes', 'inboxes')
export const listTokens = (token: string) => every<Token, 'tokens'>(token, '/v1/admin/tokens', 'tokens')

export const createToken = (token: string, body: { name: string; all_inboxes?: boolean; inboxes?: string[] }) =>
  request<TokenCreated>(token, '/v1/admin/tokens', { method: 'POST', body: JSON.stringify(body) })

export const revokeToken = (token: string, id: number) => request<void>(token, `/v1/admin/tokens/${id}`, { method: 'DELETE' })

// recentTimes returns when an inbox's newest messages arrived, for its activity line. It
// asks for summaries only, so no message bodies are read.
export async function recentTimes(token: string, slug: string, lastSeq: number, limit = 200): Promise<string[]> {
  if (lastSeq < 1) return []
  const page = await request<MessagePage>(
    token,
    `/v1/inboxes/${encodeURIComponent(slug)}/messages?before=${lastSeq + 1}&limit=${limit}&fields=summary`,
  )
  return page.messages.map((m) => m.received_at)
}
