// Colour family per status (StatusBadge, dots in the sidebar and tables).
export type Tone = 'ok' | 'warn' | 'bad' | 'info' | 'idle'

const tones: Record<string, Tone> = {
  running: 'ok',
  healthy: 'ok',
  up_to_date: 'ok',
  ok: 'ok',
  succeeded: 'ok',
  applied: 'ok',
  restarting: 'warn',
  starting: 'warn',
  partial: 'warn',
  update_available: 'warn',
  pulled_not_applied: 'warn',
  base_updated: 'warn',
  running_job: 'warn',
  queued: 'warn',
  removing: 'warn',
  created: 'idle',
  unhealthy: 'bad',
  failed: 'bad',
  invalid: 'bad',
  dead: 'bad',
  error: 'bad',
  rolled_back: 'bad',
  cancelled: 'idle',
  paused: 'info',
  local_build: 'info',
  pinned_digest: 'info',
  orphaned: 'info',
  stopped: 'idle',
  down: 'idle',
  exited: 'idle',
  unknown: 'idle',
}

// Statuses that show a pulsing dot (something is happening right now).
const busy = new Set(['restarting', 'starting', 'queued', 'running_job', 'removing'])

export function tone(status: string, exitCode?: number): Tone {
  if (status === 'exited' && exitCode !== undefined && exitCode !== 0) return 'bad'
  return tones[status] ?? 'idle'
}

export const isBusy = (status: string) => busy.has(status)

export const badgeClass: Record<Tone, string> = {
  ok: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300',
  warn: 'bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300',
  bad: 'bg-red-100 text-red-800 dark:bg-red-950 dark:text-red-300',
  info: 'bg-sky-100 text-sky-800 dark:bg-sky-950 dark:text-sky-300',
  idle: 'bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400',
}

export const dotClass: Record<Tone, string> = {
  ok: 'bg-emerald-500',
  warn: 'bg-amber-500',
  bad: 'bg-red-500',
  info: 'bg-sky-500',
  idle: 'bg-zinc-400 dark:bg-zinc-500',
}

// Human label: "update_available" → "update available".
export const label = (status: string) => status.replace(/_/g, ' ')
