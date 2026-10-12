import { describe, expect, it } from 'vitest'
import { ansi, stripAnsi } from './ansi'

describe('ansi', () => {
  it('passes plain text through', () => {
    expect(ansi('plain')).toEqual([{ text: 'plain', cls: '' }])
  })
  it('turns colours and bold into classes and resets', () => {
    expect(ansi('\x1b[1;31mERROR\x1b[0m done')).toEqual([
      { text: 'ERROR', cls: 'text-red-400 font-bold' },
      { text: ' done', cls: '' },
    ])
  })
  it('drops other escapes', () => {
    expect(stripAnsi('\x1b[2K\x1b[32mok\x1b[m')).toBe('ok')
  })
})
