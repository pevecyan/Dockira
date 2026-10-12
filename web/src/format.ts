// Formatting helpers shared by the views.

export function bytes(n: number | undefined | null, digits = 1): string {
  if (n === undefined || n === null || !isFinite(n)) return '–'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  let v = Math.abs(n)
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  const d = i === 0 || v >= 100 ? 0 : digits
  return `${n < 0 ? '-' : ''}${v.toFixed(d)} ${units[i]}`
}

export const gib = (n: number) => (n / 2 ** 30).toFixed(n >= 10 * 2 ** 30 ? 0 : 1)

export function pct(n: number | undefined | null, digits = 0): string {
  if (n === undefined || n === null || !isFinite(n)) return '–'
  return `${n.toFixed(n > 0 && n < 1 && digits === 0 ? 1 : digits)}%`
}

// "3 min ago", "in 2 h"; past and future.
export function ago(t: string | number | Date | undefined | null, now = Date.now()): string {
  if (!t) return '–'
  const ms = typeof t === 'number' ? t : new Date(t).getTime()
  if (!isFinite(ms) || ms <= 0) return '–'
  const d = Math.round((now - ms) / 1000)
  const abs = Math.abs(d)
  let s: string
  if (abs < 45) s = `${abs} s`
  else if (abs < 45 * 60) s = `${Math.round(abs / 60)} min`
  else if (abs < 36 * 3600) s = `${Math.round(abs / 3600)} h`
  else s = `${Math.round(abs / 86400)} d`
  if (abs < 5) return 'just now'
  return d >= 0 ? `${s} ago` : `in ${s}`
}

// Seconds → "1 h 12 min", "45 s".
export function duration(sec: number | undefined | null): string {
  if (sec === undefined || sec === null || !isFinite(sec) || sec < 0) return '–'
  if (sec < 60) return `${Math.round(sec)} s`
  const m = Math.floor(sec / 60)
  if (m < 60) return `${m} min`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h} h ${m % 60} min`
  return `${Math.floor(h / 24)} d ${h % 24} h`
}

export function since(t: string | undefined | null, now = Date.now()): string {
  if (!t || t.startsWith('0001')) return '–'
  return duration((now - new Date(t).getTime()) / 1000)
}

export function time(t: string | undefined | null): string {
  if (!t) return ''
  const d = new Date(t)
  return isFinite(d.getTime()) ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false }) : ''
}

export function dateTime(t: string | undefined | null): string {
  if (!t || t.startsWith('0001')) return '–'
  const d = new Date(t)
  return isFinite(d.getTime()) ? d.toLocaleString([], { dateStyle: 'medium', timeStyle: 'short', hour12: false }) : '–'
}

// "docker.io/library/postgres:16" → "postgres:16"; long registries kept, digests shortened.
export function shortImage(ref: string | undefined | null): string {
  if (!ref) return ''
  let r = ref.replace(/^(docker\.io|index\.docker\.io)\/(library\/)?/, '')
  const at = r.indexOf('@sha256:')
  if (at >= 0) r = r.slice(0, at) + '@' + r.slice(at + 8, at + 20)
  return r
}

export const shortDigest = (d: string | undefined | null) => (d ? d.replace(/^sha256:/, '').slice(0, 12) : '')

// Stack IDs contain slashes ("tools/scanner-service"): one URL path segment.
export const enc = (id: string) => encodeURIComponent(id)

export function plural(n: number, one: string, many = one + 's') {
  return `${n} ${n === 1 ? one : many}`
}
