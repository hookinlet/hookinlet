import { describe, expect, it } from 'vitest'

// router.ts reads window.location when it is loaded, so the pure parser is tested through a
// stub of the one global it touches.
;(globalThis as unknown as { window: unknown }).window = {
  location: { pathname: '/admin/' },
  addEventListener: () => {},
  history: { pushState: () => {} },
  scrollTo: () => {},
}
const { parse, href } = await import('./router')

describe('parse', () => {
  it('finds a screen under /admin', () => {
    expect(parse('/admin/')).toEqual({ name: 'overview' })
    expect(parse('/admin')).toEqual({ name: 'overview' })
    expect(parse('/admin/inboxes')).toEqual({ name: 'inboxes' })
    expect(parse('/admin/tokens/')).toEqual({ name: 'tokens' })
  })
  it('does not invent screens', () => {
    expect(parse('/admin/nope')).toEqual({ name: 'notfound' })
  })
})

describe('href', () => {
  it('puts every link under /admin', () => {
    expect(href('/')).toBe('/admin/')
    expect(href('/tokens')).toBe('/admin/tokens')
  })
})
