import { reactive } from 'vue'
import {
  api, onConfirmRequired, onLoginRequired,
  type Container, type ContainerStats, type FeedItem, type HostStats, type Info, type Job, type ServiceUpdate, type Stack, type StackAction,
  type UpdatesSummary,
} from './api'

export interface Toast {
  id: number
  tone: 'ok' | 'warn' | 'bad' | 'info'
  text: string
  jobId?: number
  sticky?: boolean
}

export interface Dialog {
  title: string
  text: string
  confirm: string
  danger?: boolean
  typed?: string // the user must type this to confirm (down -v)
  resolve: (ok: boolean) => void
}

// Global state fed by REST calls and the /api/events stream (one reactive object).
export const state = reactive({
  info: null as Info | null,
  loginRequired: false,
  canLogout: false,
  eventsConnected: false,
  newVersion: '' as string, // the server was updated since this page loaded
  error: '' as string,

  stacks: [] as Stack[],
  standalone: [] as Container[],
  dockerError: '' as string,
  scannedAt: '' as string,
  stacksLoaded: false,

  host: null as HostStats | null,
  cpuHistory: [] as number[], // host CPU % samples for the sparkline
  cstats: {} as Record<string, ContainerStats>,

  updates: null as UpdatesSummary | null,
  serviceUpdates: [] as ServiceUpdate[],
  feed: [] as FeedItem[],
  jobs: {} as Record<number, Job>, // jobs seen live in this session
  jobPanel: null as number | null, // job whose output is open
  toasts: [] as Toast[],
  dialog: null as Dialog | null,
  passwordPrompt: null as ((ok: boolean) => void) | null,
  textPrompt: null as { title: string; label: string; value: string; placeholder?: string; resolve: (v: string | null) => void } | null,
})

let events: EventSource | null = null
let toastSeq = 0

onLoginRequired(() => {
  state.loginRequired = true
  events?.close()
  state.eventsConnected = false
})

// 403 {confirm: true}: ask for the password, then the request is retried.
onConfirmRequired(() => new Promise<boolean>((resolve) => (state.passwordPrompt = resolve)))

export async function init() {
  try {
    const s = await api.session()
    if (!s.authenticated) {
      state.loginRequired = true
      return
    }
    state.canLogout = true
    state.loginRequired = false
    state.info = await api.info()
    state.error = ''
  } catch (e: any) {
    if (!state.loginRequired) state.error = e.message
    return
  }
  subscribe()
  api.feed().then((f) => (state.feed = f.items)).catch(() => {})
  loadUpdates()
  api.jobs({ limit: 20 }).then((l) => {
    for (const j of l.active) state.jobs[j.id] = j
  }).catch(() => {})
}

export async function signOut() {
  try {
    await api.logout()
  } finally {
    window.location.href = '/'
  }
}

let retry: number | undefined
let retryDelay = 1000

function subscribe() {
  events?.close()
  window.clearTimeout(retry)
  const es = new EventSource('/api/events')
  events = es
  es.onopen = () => {
    state.eventsConnected = true
    retryDelay = 1000
  }
  es.onerror = () => {
    state.eventsConnected = false
    if (es.readyState !== EventSource.CLOSED) return // the browser retries by itself
    // The browser gave up (the server was restarted, e.g. by a deploy): retry ourselves, unless
    // the session ended, then show the login form.
    api.session()
      .then((s) => {
        if (!s.authenticated) state.loginRequired = true
        else subscribe()
      })
      .catch(() => {
        retry = window.setTimeout(subscribe, retryDelay)
        retryDelay = Math.min(retryDelay * 2, 15000)
      })
  }
  const on = (name: string, fn: (d: any) => void) => es.addEventListener(name, (e) => fn(JSON.parse((e as MessageEvent).data)))
  on('hello', (d: { version: string }) => {
    state.eventsConnected = true
    // reconnected to a newer server (deployed while this page was open): offer a reload
    if (state.info && d.version && d.version !== state.info.version) state.newVersion = d.version
  })
  on('stacks', (d) => {
    state.stacks = d.stacks
    state.standalone = d.standalone
    state.dockerError = d.docker_error ?? ''
    state.scannedAt = d.scanned_at
    state.stacksLoaded = true
  })
  on('host', (h: HostStats) => {
    state.host = h
    state.cpuHistory.push(h.cpu_pct)
    if (state.cpuHistory.length > 60) state.cpuHistory.splice(0, state.cpuHistory.length - 60)
  })
  on('cstats', (c) => (state.cstats = c ?? {}))
  on('feed', (it: FeedItem) => {
    state.feed.push(it)
    if (state.feed.length > 300) state.feed.splice(0, state.feed.length - 300)
  })
  on('job', (j: Job) => trackJob(j))
  on('updates', (u: UpdatesSummary) => {
    state.updates = u
    window.clearTimeout(updatesTimer)
    updatesTimer = window.setTimeout(loadUpdates, 300)
  })
  on('dropped', () => reload())
}

let updatesTimer: number | undefined

export async function loadUpdates() {
  try {
    const u = await api.updates()
    state.updates = u.summary
    state.serviceUpdates = u.services
  } catch {
    /* the card shows "not checked" */
  }
}

// the update state of a stack's services
export const updatesFor = (stackId: string) => state.serviceUpdates.filter((u) => u.stack_id === stackId)
export const hasUpdate = (u: ServiceUpdate) => u.state === 'update_available' || u.state === 'pulled_not_applied' || u.state === 'base_updated'

export async function reload() {
  try {
    const l = await api.stacks()
    state.stacks = l.stacks
    state.standalone = l.standalone
    state.dockerError = l.docker_error ?? ''
    state.stacksLoaded = true
  } catch (e: any) {
    toast('bad', e.message)
  }
}

export function toast(tone: Toast['tone'], text: string, extra: Partial<Toast> = {}) {
  const t: Toast = { id: ++toastSeq, tone, text, ...extra }
  state.toasts.push(t)
  if (!t.sticky) setTimeout(() => dismiss(t.id), tone === 'bad' ? 9000 : 5000)
  return t.id
}

export function dismiss(id: number) {
  const i = state.toasts.findIndex((t) => t.id === id)
  if (i >= 0) state.toasts.splice(i, 1)
}

// trackJob keeps job state and turns it into a toast that follows the job.
function trackJob(j: Job) {
  const known = state.jobs[j.id]
  state.jobs[j.id] = j
  const existing = state.toasts.find((t) => t.jobId === j.id)
  const done = j.state === 'succeeded' || j.state === 'failed' || j.state === 'cancelled'
  if (!existing && !known && done) return // finished before we saw it: no toast
  const tone = j.state === 'succeeded' ? 'ok' : j.state === 'failed' ? 'bad' : j.state === 'cancelled' ? 'warn' : 'info'
  const text = done ? `${j.title}: ${j.state === 'failed' ? j.error || 'failed' : j.state}` : `${j.title}…`
  if (existing) {
    existing.tone = tone
    existing.text = text
    if (done) {
      existing.sticky = false
      setTimeout(() => dismiss(existing.id), j.state === 'failed' ? 12000 : 5000)
    }
  } else {
    toast(tone, text, { jobId: j.id, sticky: !done })
  }
}

export const activeJobs = () => Object.values(state.jobs).filter((j) => j.state === 'queued' || j.state === 'running')

export function jobFor(stackId: string): Job | undefined {
  return activeJobs().find((j) => j.stack_id === stackId)
}

export function promptText(p: { title: string; label: string; value?: string; placeholder?: string }): Promise<string | null> {
  return new Promise((resolve) => (state.textPrompt = { ...p, value: p.value ?? '', resolve }))
}

// confirmPassword asks for the password unless it was confirmed in the last minutes.
export async function confirmPassword(): Promise<boolean> {
  const s = await api.session()
  if (s.confirmed) return true
  return new Promise((resolve) => (state.passwordPrompt = resolve))
}

export function ask(d: Omit<Dialog, 'resolve'>): Promise<boolean> {
  return new Promise((resolve) => (state.dialog = { ...d, resolve }))
}

const confirmText: Partial<Record<StackAction, (name: string) => Omit<Dialog, 'resolve'>>> = {
  stop: (n) => ({ title: `Stop ${n}?`, text: 'Its containers stop until you start them again.', confirm: 'Stop', danger: true }),
  down: (n) => ({ title: `Take down ${n}?`, text: 'Containers and networks are removed. Volumes and images stay, so `up` brings it back.', confirm: 'Take down', danger: true }),
  down_volumes: (n) => ({ title: `Take down ${n} and delete its volumes?`, text: 'Named volumes (databases, uploads…) are deleted for good. This cannot be undone.', confirm: 'Delete volumes', danger: true, typed: n }),
  recreate: (n) => ({ title: `Recreate ${n}?`, text: 'Containers are removed and created again from the current compose file.', confirm: 'Recreate' }),
}

// runAction asks when the action is destructive, starts the job and lets the toast follow it.
export async function runAction(stack: Stack, action: StackAction, services?: string[]): Promise<Job | null> {
  const c = confirmText[action]
  const name = services?.length ? `${services.join(', ')} (${stack.name})` : stack.name
  if (c && !(await ask(c(name)))) return null
  try {
    const j = await api.stackAction(stack.id, action, services)
    trackJob(j)
    return j
  } catch (e: any) {
    toast('bad', e.message)
    return null
  }
}

// Stack CPU/RAM: the sum over its running containers.
export function stackUsage(st: Stack): { cpu: number; mem: number } {
  let cpu = 0
  let mem = 0
  for (const sv of st.services) {
    for (const c of sv.containers) {
      const s = state.cstats[c.id]
      if (s) {
        cpu += s.cpu_pct
        mem += s.mem_bytes
      }
    }
  }
  return { cpu, mem }
}
