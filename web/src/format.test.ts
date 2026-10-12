import { describe, expect, it } from 'vitest'
import { ago, bytes, compact, visible } from './format'

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
  })
})
