<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { api, urls, type Job } from '../api'
import { ago, duration } from '../format'
import { toast } from '../state'
import StatusBadge from './StatusBadge.vue'

// Live output of one job (SSE), then its final state. Used in the job panel and on the Jobs page.
const props = defineProps<{ id: number; height?: string }>()
const lines = ref<string[]>([])
const job = ref<Job | null>(null)
const box = ref<HTMLElement | null>(null)
let es: EventSource | null = null

function open() {
  es?.close()
  lines.value = []
  job.value = null
  api.job(props.id).then((d) => (job.value = job.value ?? d.job)).catch(() => {})
  es = new EventSource(urls.jobStream(props.id))
  es.addEventListener('lines', (e) => {
    const l: string[] = JSON.parse((e as MessageEvent).data)
    lines.value.push(...l.filter((x) => x !== ''))
    if (lines.value.length > 5000) lines.value.splice(0, lines.value.length - 5000)
    scroll()
  })
  es.addEventListener('done', (e) => {
    job.value = JSON.parse((e as MessageEvent).data)
    es?.close()
  })
  es.onerror = () => es?.close()
}

async function scroll() {
  const el = box.value
  if (!el) return
  const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60
  await nextTick()
  if (atBottom) el.scrollTop = el.scrollHeight
}

async function cancel() {
  try {
    await api.cancelJob(props.id)
  } catch (e: any) {
    toast('bad', e.message)
  }
}

watch(() => props.id, open, { immediate: true })
onBeforeUnmount(() => es?.close())

const lineClass = (l: string) =>
  l.startsWith('▸') ? 'text-sky-300 font-semibold' : /\b(error|failed|denied|no such)\b/i.test(l) ? 'text-red-400' : /\bwarn/i.test(l) ? 'text-amber-300' : ''
</script>

<template>
  <div class="flex min-h-0 flex-col">
    <div v-if="job" class="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-zinc-200 px-4 py-2 text-xs dark:border-zinc-800">
      <StatusBadge :status="job.state === 'running' ? 'running_job' : job.state" :text="job.state" />
      <span class="muted">#{{ job.id }} · {{ job.trigger }} · started {{ ago(job.started_at || job.created_at) }}</span>
      <span v-if="job.ended_at && job.started_at" class="muted">· took {{ duration((Date.parse(job.ended_at) - Date.parse(job.started_at)) / 1000) }}</span>
      <span v-if="job.error" class="text-red-600 dark:text-red-400">{{ job.error }}</span>
      <button v-if="job.state === 'running' || job.state === 'queued'" class="btn btn-sm ml-auto" @click="cancel">Cancel</button>
    </div>
    <div ref="box" class="min-h-0 flex-1 overflow-auto bg-zinc-950 px-3 py-2 font-mono text-[11.5px] leading-[1.55] text-zinc-300" :style="{ height: height ?? '24rem' }">
      <div v-for="(l, i) in lines" :key="i" class="whitespace-pre-wrap break-all" :class="lineClass(l)">{{ l }}</div>
      <div v-if="!lines.length" class="text-zinc-500">{{ job && job.state === 'queued' ? 'Waiting for another job on this stack…' : 'No output yet.' }}</div>
    </div>
  </div>
</template>
