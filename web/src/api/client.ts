import type { components } from './schema'

export type Status = components['schemas']['AdminStatus']

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }
}

// request calls the admin API with the admin token. The token lives in the caller's
// memory only; it is never put in a URL.
export async function request<T>(token: string, path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { ...init.headers, Authorization: `Bearer ${token}`, Accept: 'application/json' },
  })
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
  return (await res.json()) as T
}

export const getStatus = (token: string) => request<Status>(token, '/v1/admin/status')
