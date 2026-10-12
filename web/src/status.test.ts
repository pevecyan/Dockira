import { describe, expect, it } from 'vitest'
import { isBusy, label, tone } from './status'

describe('tone', () => {
  it('maps docker and update states to colour families', () => {
    expect(tone('running')).toBe('ok')
    expect(tone('update_available')).toBe('warn')
    expect(tone('unhealthy')).toBe('bad')
    expect(tone('local_build')).toBe('info')
    expect(tone('down')).toBe('idle')
    expect(tone('something-new')).toBe('idle')
  })
  it('treats a non-zero exit as bad and a clean exit as idle', () => {
    expect(tone('exited', 137)).toBe('bad')
    expect(tone('exited', 0)).toBe('idle')
    expect(tone('exited')).toBe('idle')
  })
})

describe('helpers', () => {
  it('labels and busy states', () => {
    expect(label('pulled_not_applied')).toBe('pulled not applied')
    expect(isBusy('restarting')).toBe(true)
    expect(isBusy('running')).toBe(false)
  })
})
