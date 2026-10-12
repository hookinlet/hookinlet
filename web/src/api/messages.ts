import { ApiError, request, type Inbox, type MessagePage } from './client'
import type { components } from './schema'
import { SSEParser } from '../sse'

export type Message = components['schemas']['Message']
export type StreamGap = components['schemas']['StreamGap']

export interface Filters {
  type: string
  verified: '' | 'true' | 'false'
  since: string // an ISO time, or ''
}

const enc = encodeURIComponent

export const getInbox = (token: string, slug: string) => request<Inbox>(token, `/v1/admin/inboxes/${enc(slug)}`)

function query(f: Filters, extra: Record<string, string | number>): string {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(extra)) q.set(k, String(v))
  if (f.type) q.set('type', f.type)
  if (f.verified) q.set('verified', f.verified)
  if (f.since) q.set('since', f.since)
  return q.toString()
}

// newest reads a page of summaries, newest first. before is the seq to read below, or 0 for the
// very newest. Bodies are left out: a page of them could be large, and the list does not show them.
export function newest(token: string, slug: string, f: Filters, before: number, limit = 50) {
  const extra: Record<string, string | number> = { limit, fields: 'summary' }
  extra.before = before > 0 ? before : Number.MAX_SAFE_INTEGER
  return request<MessagePage>(token, `/v1/inboxes/${enc(slug)}/messages?${query(f, extra)}`)
}

export const getMessage = (token: string, slug: string, seq: number) =>
  request<Message>(token, `/v1/inboxes/${enc(slug)}/messages/${seq}`)

// rawBytes reads the exact bytes of a delivery.
export async function rawBytes(token: string, slug: string, seq: number): Promise<Uint8Array> {
  const res = await fetch(`/v1/inboxes/${enc(slug)}/messages/${seq}/raw`, { headers: { Authorization: `Bearer ${token}` } })
  if (!res.ok) throw new ApiError(res.status, res.statusText)
  return new Uint8Array(await res.arrayBuffer())
}

export interface StreamHandlers {
  message: (m: Message) => void
  gap: (g: StreamGap) => void
  state: (s: 'connecting' | 'live' | 'retrying') => void
  unauthorized: () => void
  // failed means the stream cannot work (the inbox is gone, a filter is refused): retrying would
  // only repeat the answer, so the loop ends and says why.
  failed: (reason: string) => void
}

// follow reads the live stream of an inbox until signal aborts. It reconnects after a drop,
// resuming from the last seq it saw (Last-Event-ID), so a message is neither missed nor
// delivered twice. A 401 ends it and is reported: the token is no longer good.
export async function follow(token: string, slug: string, f: Filters, after: number, h: StreamHandlers, signal: AbortSignal): Promise<void> {
  let last = after
  let delay = 1000
  while (!signal.aborted) {
    h.state('connecting')
    const parser = new SSEParser()
    try {
      const q = new URLSearchParams({ after: String(last) })
      if (f.type) q.set('type', f.type)
      if (f.verified) q.set('verified', f.verified)
      const res = await fetch(`/v1/inboxes/${enc(slug)}/stream?${q}`, {
        headers: { Authorization: `Bearer ${token}`, Accept: 'text/event-stream', 'Last-Event-ID': String(last) },
        signal,
      })
      if (res.status === 401) return h.unauthorized()
      if (res.status >= 400 && res.status < 500 && res.status !== 408 && res.status !== 429) {
        return h.failed(res.status === 404 ? 'This inbox no longer exists.' : `The server refused the stream (${res.status}).`)
      }
      if (!res.ok || !res.body) throw new ApiError(res.status, res.statusText)
      h.state('live')
      delay = 1000
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
      for (;;) {
        const { value, done } = await reader.read()
        if (done) break
        for (const ev of parser.push(value)) {
          if (ev.event === 'message') {
            const m = JSON.parse(ev.data) as Message
            last = Math.max(last, m.seq)
            h.message(m)
          } else if (ev.event === 'gap') {
            const g = JSON.parse(ev.data) as StreamGap
            // The messages before oldest_seq are gone; the stream carries on from there, and a
            // reconnect must not ask for them again.
            last = Math.max(last, g.oldest_seq - 1)
            h.gap(g)
          }
        }
        if (parser.retry !== null) delay = parser.retry
      }
    } catch (e) {
      if (signal.aborted) return
      void e // a dropped connection: fall through to the wait and reconnect
    }
    h.state('retrying')
    await new Promise<void>((resolve) => {
      const t = window.setTimeout(resolve, delay)
      signal.addEventListener('abort', () => (window.clearTimeout(t), resolve()), { once: true })
    })
    delay = Math.min(delay * 2, 15000)
  }
}
