import { describe, expect, it } from 'vitest'
import { bytes } from './format'

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
