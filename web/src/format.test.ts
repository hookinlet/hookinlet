import { describe, expect, it } from 'vitest'
import { ago, bytes, compact, visible, visibleInBody } from './format'

describe('bytes', () => {
  it('writes sizes for people', () => {
    expect(bytes(0)).toBe('0 B')
    expect(bytes(999)).toBe('999 B')
    expect(bytes(1800000000)).toBe('1.8 GB')
    expect(bytes(212000000000)).toBe('212 GB')
  })
  it('does not print nonsense', () => {
    expect(bytes(-1)).toBe('-')
    expect(bytes(Number.NaN)).toBe('-')
  })
})

describe('compact', () => {
  it('shortens counts', () => {
    expect(compact(341)).toBe('341')
    expect(compact(1000)).toBe('1K')
    expect(compact(88016)).toBe('88K')
    expect(compact(4200000)).toBe('4.2M')
    expect(compact(2_104_882)).toBe('2.1M')
    expect(compact(999_999)).toBe('1M')
    expect(compact(999_500_000)).toBe('1B')
  })
})

describe('ago', () => {
  const now = new Date('2026-10-12T12:00:00Z')
  it('speaks in the largest sensible unit', () => {
    expect(ago(new Date('2026-10-12T11:59:59Z'), now)).toBe('just now')
    expect(ago(new Date('2026-10-12T11:59:20Z'), now)).toBe('40 s ago')
    expect(ago(new Date('2026-10-12T11:46:00Z'), now)).toBe('14 min ago')
    expect(ago(new Date('2026-10-12T09:00:00Z'), now)).toBe('3 h ago')
    expect(ago(new Date('2026-10-09T12:00:00Z'), now)).toBe('3 days ago')
  })
  it('accepts a timestamp string and survives a bad one', () => {
    expect(ago('2026-10-12T11:46:00Z', now)).toBe('14 min ago')
    expect(ago('nope', now)).toBe('-')
  })
})

describe('visible', () => {
  it('shows invisible and direction-changing characters', () => {
    expect(visible('invoice.paid')).toBe('invoice.paid')
    expect(visible('evil\u202Efdp.exe')).toBe('evil<U+202E>fdp.exe')
    expect(visible('a\u200Bb')).toBe('a<U+200B>b')
    expect(visible('x\u061Cy\u2028z\u{E0041}')).toBe('x<U+061C>y<U+2028>z<U+E0041>')
    expect(visible('plain text, ünïcödé ok')).toBe('plain text, ünïcödé ok')
  })
})

describe('visible, more thoroughly', () => {
  it('marks fillers, variation selectors and invisible operators', () => {
    expect(visible('a\u3164b')).toBe('a<U+3164>b')
    expect(visible('a\uFE0Fb')).toBe('a<U+FE0F>b')
    expect(visible('a\u2062b')).toBe('a<U+2062>b')
    expect(visible('a\u034Fb')).toBe('a<U+034F>b')
  })
})

describe('visibleInBody', () => {
  it('marks what reorders or hides text, and leaves emoji and scripts alone', () => {
    expect(visibleInBody('k\u202Eey')).toBe('k<U+202E>ey')
    expect(visibleInBody('x\u2028y')).toBe('x<U+2028>y')
    expect(visibleInBody('\u{1F468}\u200D\u{1F469}\u200D\u{1F467} \u2764\uFE0F')).toBe('\u{1F468}\u200D\u{1F469}\u200D\u{1F467} \u2764\uFE0F')
  })
})
