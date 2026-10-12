import { describe, expect, it } from 'vitest'
import { buildConfig, mergeConfig, missing, presetOf, PRESETS, sameConfig, valuesOf } from './verifiers'

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

describe('mergeConfig', () => {
  const hmac = presetOf('hmac')!
  it('keeps a setting the form has no field for', () => {
    const existing = { header: 'X-Sig', timestamp_header: 'X-Ts', payload_template: 'v0:{timestamp}:{body}' }
    const got = mergeConfig(hmac, existing, valuesOf(existing))
    expect(got).toEqual(existing)
  })
  it('removes a field the person emptied', () => {
    expect(mergeConfig(hmac, { header: 'X-Sig', prefix: 'sha256=', payload_template: 'x' }, { header: 'X-Sig', prefix: '' })).toEqual({ header: 'X-Sig', payload_template: 'x' })
  })
})

describe('sameConfig', () => {
  it('ignores key order and treats nothing as empty', () => {
    expect(sameConfig({ a: 1, b: 2 }, { b: 2, a: 1 })).toBe(true)
    expect(sameConfig(undefined, {})).toBe(true)
    expect(sameConfig({ a: 1 }, { a: 2 })).toBe(false)
  })
})
