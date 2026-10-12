// Minimal ANSI SGR → spans (colours and bold); everything else is stripped. Container logs
// often carry colour codes (nginx, node, python loggers), and raw escapes are unreadable.

export interface Seg {
  text: string
  cls: string
}

const fg: Record<number, string> = {
  30: 'text-zinc-500', 31: 'text-red-400', 32: 'text-emerald-400', 33: 'text-amber-300', 34: 'text-sky-400',
  35: 'text-fuchsia-400', 36: 'text-cyan-300', 37: 'text-zinc-200',
  90: 'text-zinc-500', 91: 'text-red-300', 92: 'text-emerald-300', 93: 'text-amber-200', 94: 'text-sky-300',
  95: 'text-fuchsia-300', 96: 'text-cyan-200', 97: 'text-white',
}

// eslint-disable-next-line no-control-regex
const esc = /\x1b\[([0-9;]*)([A-Za-z])/g

export function hasAnsi(s: string) {
  return s.includes('\x1b[')
}

export function ansi(s: string): Seg[] {
  if (!hasAnsi(s)) return [{ text: s, cls: '' }]
  const out: Seg[] = []
  let color = ''
  let bold = false
  let last = 0
  for (const m of s.matchAll(esc)) {
    if (m.index! > last) out.push({ text: s.slice(last, m.index), cls: [color, bold ? 'font-bold' : ''].join(' ').trim() })
    last = m.index! + m[0].length
    if (m[2] !== 'm') continue
    for (const p of (m[1] || '0').split(';').map(Number)) {
      if (p === 0) (color = ''), (bold = false)
      else if (p === 1) bold = true
      else if (p === 22) bold = false
      else if (p === 39) color = ''
      else if (fg[p]) color = fg[p]
    }
  }
  if (last < s.length) out.push({ text: s.slice(last), cls: [color, bold ? 'font-bold' : ''].join(' ').trim() })
  return out.filter((x) => x.text !== '')
}

export const stripAnsi = (s: string) => s.replace(esc, '')
