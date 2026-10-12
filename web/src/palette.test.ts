import { describe, expect, it } from 'vitest'
import { filterCommands, score, type Command } from './palette'

const cmd = (label: string, hint?: string): Command => ({ id: label, label, hint, run: () => {} })

describe('score', () => {
  it('prefers the start of a label, then a word, then the middle, then scattered letters', () => {
    expect(score('Tokens', 'tok')).toBeGreaterThan(score('Open inbox stripe-prod', 'stripe'))
    expect(score('Open inbox stripe-prod', 'stripe')).toBeGreaterThan(score('Open inbox mystripe', 'stripe'))
    expect(score('Open inbox mystripe', 'stripe')).toBeGreaterThan(score('Run retention now', 'rtn'))
    expect(score('Run retention now', 'zzz')).toBe(0)
  })
  it('matches everything when there is nothing typed', () => {
    expect(score('anything', '  ')).toBeGreaterThan(0)
  })
})

describe('filterCommands', () => {
  const all = [cmd('Go to Overview'), cmd('Go to Tokens'), cmd('Open inbox stripe-prod'), cmd('Open inbox github-demo'), cmd('Sign out')]
  it('keeps the order when nothing is typed', () => {
    expect(filterCommands(all, '').map((c) => c.label)).toEqual(all.map((c) => c.label))
  })
  it('finds and ranks', () => {
    expect(filterCommands(all, 'stripe').map((c) => c.label)).toEqual(['Open inbox stripe-prod'])
    expect(filterCommands(all, 'to')[0]?.label).toBe('Go to Overview')
  })
  it('can match on the hint', () => {
    expect(filterCommands([cmd('Open inbox x', 'billing')], 'billing')).toHaveLength(1)
  })
})
