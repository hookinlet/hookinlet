import { describe, expect, it } from 'vitest'
import { buildConfig, missing, presetOf, PRESETS, valuesOf } from './verifiers'

describe('verifier presets', () => {
  it('cover every verifier the server ships', () => {
    expect(PRESETS.map((p) => p.id).sort()).toEqual(['github', 'hmac', 'none', 'slack', 'standardwebhooks', 'stripe', 'token'])
  })
  it('say which ones need a secret', () => {
    expect(presetOf('none')?.secret).toBe(false)
    expect(presetOf('stripe')?.secret).not.toBe(false)
  })
})

describe('buildConfig', () => {
  const hmac = presetOf('hmac')!
  it('sends only what was filled in, numbers as numbers', () => {
    expect(buildConfig(hmac, { header: ' X-Sig ', prefix: '', tolerance_seconds: '120', nonsense: 'x' })).toEqual({ header: 'X-Sig', tolerance_seconds: 120 })
  })
  it('sends nothing for a preset with no settings', () => {
    expect(buildConfig(presetOf('github')!, { header: 'x' })).toEqual({})
  })
})

describe('missing', () => {
  it('names a required field left empty', () => {
    expect(missing(presetOf('hmac')!, { header: ' ' })).toEqual(['Signature header'])
    expect(missing(presetOf('hmac')!, { header: 'X-Sig' })).toEqual([])
  })
})

describe('valuesOf', () => {
  it('turns a stored configuration back into form values', () => {
    expect(valuesOf({ header: 'X-Sig', tolerance_seconds: 120 })).toEqual({ header: 'X-Sig', tolerance_seconds: '120' })
    expect(valuesOf(undefined)).toEqual({})
  })
})
