import { describe, expect, it } from 'vitest'
import { buckets, HOURS } from './activity'

describe('buckets', () => {
  const now = new Date('2026-10-12T12:00:00Z')
  it('counts into hourly buckets, oldest first', () => {
    const got = buckets(['2026-10-12T11:30:00Z', '2026-10-12T11:59:00Z', '2026-10-12T10:30:00Z', '2026-10-11T12:30:00Z'], now)
    expect(got).toHaveLength(HOURS)
    expect(got[HOURS - 1]).toBe(2) // the last hour
    expect(got[HOURS - 2]).toBe(1)
    expect(got[0]).toBe(1) // 23.5 hours ago
  })
  it('ignores what is outside the window or not a time', () => {
    const got = buckets(['2026-10-10T00:00:00Z', '2026-10-12T13:00:00Z', 'nope'], now)
    expect(got.reduce((a, b) => a + b, 0)).toBe(0)
  })
})
