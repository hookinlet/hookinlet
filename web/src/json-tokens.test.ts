import { describe, expect, it } from 'vitest'
import { tokens } from './json-tokens'

const text = (v: unknown) => tokens(v).tokens.map((t) => t.text).join('')

describe('tokens', () => {
  it('lays out like JSON.stringify with two-space indents', () => {
    const v = { a: 1, b: [true, null, 'x'], c: {}, d: [] }
    expect(text(v)).toBe(JSON.stringify(v, null, 2))
  })
  it('keeps markup in strings as text', () => {
    const v = { html: '<img src=x onerror=alert(1)>' }
    const t = tokens(v).tokens
    expect(t.find((x) => x.kind === 'str')?.text).toBe('"<img src=x onerror=alert(1)>"')
  })
  it('stops early on a huge body and says so', () => {
    const big = Array.from({ length: 30000 }, (_, i) => i)
    const r = tokens(big)
    expect(r.truncated).toBe(true)
    expect(r.tokens.length).toBeLessThanOrEqual(20000)
  })
})
