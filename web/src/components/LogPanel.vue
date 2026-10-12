<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { urls, type LogLine } from '../api'
import { ansi, stripAnsi } from '../ansi'

// Live logs of a stack (merged, one colour per service) or of one container.
const props = withDefaults(
  defineProps<{ stack?: string; container?: string; services?: string[]; height?: string; title?: string }>(),
  { height: '28rem', title: 'Logs' },
)

interface Row extends LogLine {
  info?: boolean
}

const MAX = 5000
const lines = ref<Row[]>([])
const filter = ref('')
const paused = ref(false)
const connected = ref(false)
const ended = ref(false)
const timestamps = ref(false)
const wrap = ref(true)
const errorsOnly = ref(false)
const tail = ref('200')
const hidden = ref<Set<string>>(new Set())
const box = ref<HTMLElement | null>(null)
let es: EventSource | null = null
let pending: Row[] = []

function open() {
  es?.close()
  lines.value = []
  pending = []
  ended.value = false
  es = new EventSource(urls.logs({ stack: props.stack, container: props.container, tail: tail.value }))
  es.onopen = () => (connected.value = true)
  es.onerror = () => (connected.value = false)
  es.addEventListener('lines', (e) => {
    const batch: Row[] = JSON.parse((e as MessageEvent).data)
    if (paused.value) {
      pending.push(...batch)
      if (pending.length > MAX) pending.splice(0, pending.length - MAX)
      return
    }
    push(batch)
  })
  es.addEventListener('info', (e) => {
    const d = JSON.parse((e as MessageEvent).data)
    push([{ c: '', s: '', t: new Date().toISOString(), m: d.info, info: true }])
  })
  es.addEventListener('end', () => {
    ended.value = true
    es?.close()
  })
}

function push(batch: Row[]) {
  lines.value.push(...batch)
  if (lines.value.length > MAX) lines.value.splice(0, lines.value.length - MAX)
}

watch(paused, (p) => {
  if (!p && pending.length) {
    push(pending)
    pending = []
  }
})
watch(() => [props.stack, props.container, tail.value], open, { immediate: true })
onBeforeUnmount(() => es?.close())

// a stable colour per service, from the shared series palette
const palette = ['var(--series-1)', 'var(--series-2)', 'var(--series-3)', 'var(--series-4)', 'var(--series-5)', 'var(--series-7)', 'var(--series-8)', 'var(--series-6)']
const serviceNames = computed(() => {
  const names = new Set(props.services ?? [])
  for (const l of lines.value) if (l.s) names.add(l.s)
  return [...names].sort()
})
const colour = (s: string) => palette[Math.max(0, serviceNames.value.indexOf(s)) % palette.length]
const multi = computed(() => !props.container && serviceNames.value.length > 1)

function toggle(s: string) {
  const h = new Set(hidden.value)
  h.has(s) ? h.delete(s) : h.add(s)
  hidden.value = h
}

const filterRe = computed(() => {
  const f = filter.value.trim()
  if (!f) return null
  try {
    return new RegExp(f, 'i')
  } catch {
    return new RegExp(f.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'i')
  }
})

const shown = computed(() => {
  const re = filterRe.value
  const out: Row[] = []
  for (const l of lines.value) {
    if (!l.info && l.s && hidden.value.has(l.s)) continue
    if (errorsOnly.value && !l.info && !l.e && !/\b(error|err|fatal|panic|exception|fail(ed)?|warn(ing)?)\b/i.test(l.m)) continue
    if (re && !re.test(stripAnsi(l.m)) && !re.test(l.c)) continue
    out.push(l)
  }
  return out.slice(-1500)
})

// highlight filter matches inside the (ANSI-split) message
function segments(m: string) {
  const segs = ansi(m)
  const re = filterRe.value
  if (!re) return segs.map((s) => ({ ...s, hit: false }))
  const out: { text: string; cls: string; hit: boolean }[] = []
  const g = new RegExp(re.source, 'gi')
  for (const s of segs) {
    let last = 0
    for (const m2 of s.text.matchAll(g)) {
      if (!m2[0]) break
      if (m2.index! > last) out.push({ text: s.text.slice(last, m2.index), cls: s.cls, hit: false })
      out.push({ text: m2[0], cls: s.cls, hit: true })
      last = m2.index! + m2[0].length
    }
    if (last < s.text.length) out.push({ text: s.text.slice(last), cls: s.cls, hit: false })
  }
  return out
}

// colour by what a line says, not by its stream: many programs (postgres, nginx, python) log everything to stderr
const level = (l: Row) =>
  l.info ? 'text-zinc-500 italic' : /\b(error|fatal|panic|critical|exception)\b/i.test(l.m) ? 'text-red-400' : /\bwarn(ing)?\b/i.test(l.m) ? 'text-amber-300' : ''

const stick = ref(true)
function onScroll() {
  const el = box.value
  if (el) stick.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}
watch(
  () => shown.value.length && shown.value[shown.value.length - 1],
  async () => {
    await nextTick()
    if (stick.value && box.value) box.value.scrollTop = box.value.scrollHeight
  },
)
function toBottom() {
  stick.value = true
  if (box.value) box.value.scrollTop = box.value.scrollHeight
}

const downloadUrl = computed(() => urls.logsDownload({ stack: props.stack, container: props.container, services: hidden.value.size ? serviceNames.value.filter((s) => !hidden.value.has(s)).join(',') : undefined }))
const ts = (t: string) => t.slice(0, 19).replace('T', ' ')
</script>

<template>
  <div class="card flex flex-col">
    <div class="card-h flex-wrap">
      <span class="flex items-center gap-2">
        {{ title }}
        <span class="h-1.5 w-1.5 rounded-full" :class="connected && !paused ? 'bg-emerald-500' : 'bg-zinc-400'" :title="ended ? 'finished' : connected ? 'following' : 'disconnected'" />
      </span>
      <div class="flex min-w-0 flex-1 flex-wrap items-center justify-end gap-2 font-normal">
        <input v-model="filter" class="input min-w-0 !py-1 text-xs sm:!w-48" placeholder="Filter (regex)…" aria-label="Filter log lines" />
        <select v-model="tail" class="input !w-auto !py-1 text-xs" aria-label="Lines to load" title="Lines to load per container">
          <option value="100">last 100</option>
          <option value="200">last 200</option>
          <option value="1000">last 1000</option>
          <option value="5000">last 5000</option>
        </select>
        <button class="btn btn-sm" :class="paused ? '!border-amber-500' : ''" @click="paused = !paused">{{ paused ? 'Resume' : 'Pause' }}</button>
        <button class="btn btn-sm" @click="open">Reload</button>
        <a class="btn btn-sm" :href="downloadUrl" download>Download</a>
      </div>
    </div>
    <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5 border-b border-zinc-200 px-4 py-1.5 text-xs dark:border-zinc-800">
      <template v-if="multi">
        <button
          v-for="s in serviceNames"
          :key="s"
          class="badge border transition"
          :class="hidden.has(s) ? 'border-zinc-300 text-zinc-400 line-through dark:border-zinc-700' : 'border-transparent bg-zinc-100 dark:bg-zinc-800'"
          :title="hidden.has(s) ? 'Show' : 'Hide'"
          @click="toggle(s)"
        >
          <span class="h-2 w-2 rounded-full" :style="{ background: colour(s) }" />{{ s }}
        </button>
        <span class="mx-1 h-4 border-l border-zinc-200 dark:border-zinc-700" />
      </template>
      <label class="flex items-center gap-1.5"><input v-model="timestamps" type="checkbox" class="accent-emerald-600" />timestamps</label>
      <label class="flex items-center gap-1.5"><input v-model="wrap" type="checkbox" class="accent-emerald-600" />wrap</label>
      <label class="flex items-center gap-1.5"><input v-model="errorsOnly" type="checkbox" class="accent-emerald-600" />errors &amp; warnings</label>
      <span class="ml-auto text-zinc-500 tabular-nums">{{ shown.length }} lines<template v-if="paused"> · paused ({{ pending.length }} new)</template></span>
    </div>
    <div class="relative">
      <div
        ref="box"
        class="overflow-auto rounded-b-xl bg-zinc-950 px-3 py-2 font-mono text-[11.5px] leading-[1.55] text-zinc-300"
        :style="{ height }"
        @scroll="onScroll"
      >
        <div v-for="(l, i) in shown" :key="i" :class="[wrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre', level(l)]">
          <span v-if="timestamps && !l.info" class="text-zinc-500">{{ ts(l.t) }} </span>
          <span v-if="multi && !l.info" class="font-semibold" :style="{ color: colour(l.s) }">{{ l.s.padEnd(Math.min(14, Math.max(...serviceNames.map((n) => n.length)))) }} │ </span>
          <template v-for="(seg, j) in segments(l.m)" :key="j"><span :class="[seg.cls, seg.hit ? 'rounded-sm bg-amber-400/30 text-amber-100' : '']">{{ seg.text }}</span></template>
        </div>
        <div v-if="!shown.length" class="text-zinc-500">{{ lines.length ? 'No lines match.' : connected ? 'No log lines yet.' : 'Connecting…' }}</div>
      </div>
      <button v-if="!stick" class="btn btn-sm absolute right-4 bottom-3 shadow-lg" @click="toBottom">↓ Latest</button>
    </div>
  </div>
</template>
