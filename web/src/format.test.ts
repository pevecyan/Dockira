import { describe, expect, it } from 'vitest'
import { ago, bytes, duration, enc, pct, plural, shortDigest, shortImage } from './format'

describe('format', () => {
  it('bytes', () => {
    expect(bytes(0)).toBe('0 B')
    expect(bytes(1536)).toBe('1.5 KiB')
    expect(bytes(1.71 * 2 ** 30)).toBe('1.7 GiB')
    expect(bytes(250 * 2 ** 20)).toBe('250 MiB')
    expect(bytes(undefined)).toBe('–')
  })
  it('pct', () => {
    expect(pct(42.4)).toBe('42%')
    expect(pct(0.42)).toBe('0.4%')
    expect(pct(null)).toBe('–')
  })
  it('ago and duration', () => {
    const now = Date.parse('2026-10-11T12:00:00Z')
    expect(ago('2026-10-11T11:57:00Z', now)).toBe('3 min ago')
    expect(ago('2026-10-11T14:00:00Z', now)).toBe('in 2 h')
    expect(ago('2026-10-11T11:59:58Z', now)).toBe('just now')
    expect(ago('', now)).toBe('–')
    expect(duration(45)).toBe('45 s')
    expect(duration(4320)).toBe('1 h 12 min')
    expect(duration(3 * 86400 + 7200)).toBe('3 d 2 h')
  })
  it('images and ids', () => {
    expect(shortImage('docker.io/library/postgres:16')).toBe('postgres:16')
    expect(shortImage('ghcr.io/acme/notes-mcp:latest')).toBe('ghcr.io/acme/notes-mcp:latest')
    expect(shortImage('nginx@sha256:0123456789abcdef0123')).toBe('nginx@0123456789ab')
    expect(shortDigest('sha256:abcdef0123456789')).toBe('abcdef012345')
    expect(enc('tools/scanner-service')).toBe('tools%2Fscanner-service')
    expect(plural(1, 'stack')).toBe('1 stack')
    expect(plural(3, 'stack')).toBe('3 stacks')
  })
})
