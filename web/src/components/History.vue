<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api, type Revision, type Stack } from '../api'
import { ago, dateTime } from '../format'
import { ask, state, toast } from '../state'
import DiffView from './DiffView.vue'
import { langFor } from '../editor/cm'

// Every saved version of the stack's files; compare with the current file or the version before,
// and restore any of them.
const props = defineProps<{ stack: Stack }>()
const revisions = ref<Revision[]>([])
const fileFilter = ref('')
const selected = ref<Revision | null>(null)
const against = ref<'current' | 'previous'>('current')
const left = ref('')
const right = ref('')
const loading = ref(false)
const error = ref('')

async function load() {
  try {
    revisions.value = (await api.revisions(props.stack.id)).revisions
    if (!selected.value && revisions.value.length) pick(revisions.value[0])
  } catch (e: any) {
    error.value = e.message
  }
}
load()

const fileNames = computed(() => [...new Set(revisions.value.map((r) => r.file))])
const shown = computed(() => revisions.value.filter((r) => !fileFilter.value || r.file === fileFilter.value))

const reasonClass: Record<string, string> = {
  baseline: 'bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400',
  edit: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300',
  restore: 'bg-sky-100 text-sky-800 dark:bg-sky-950 dark:text-sky-300',
  external: 'bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300',
  delete: 'bg-red-100 text-red-800 dark:bg-red-950 dark:text-red-300',
}
const reasonText: Record<string, string> = { baseline: 'first seen', edit: 'edited here', restore: 'restored', external: 'changed elsewhere', delete: 'deleted' }

async function pick(r: Revision) {
  selected.value = r
  await compare()
}

async function compare() {
  const r = selected.value
  if (!r) return
  loading.value = true
  error.value = ''
  try {
    const full = await api.revision(props.stack.id, r.id)
    right.value = full.content ?? ''
    if (against.value === 'current') {
      left.value = (await api.file(props.stack.id, r.file)).content
    } else {
      const prev = revisions.value.find((x) => x.file === r.file && x.id < r.id)
      left.value = prev ? (await api.revision(props.stack.id, prev.id)).content ?? '' : ''
    }
  } catch (e: any) {
    error.value = e.message
    right.value = left.value = ''
  } finally {
    loading.value = false
  }
}
watch(against, compare)

async function restore(apply: boolean) {
  const r = selected.value
  if (!r) return
  if (!(await ask({ title: `Restore ${r.file} from ${dateTime(r.created_at)}?`, text: apply ? 'The file is written back and the stack is updated (docker compose up -d).' : 'The file is written back. The current version stays in History.', confirm: apply ? 'Restore & apply' : 'Restore' })))
    return
  try {
    const res = await api.restore(props.stack.id, r.id, apply)
    if (res.job) state.jobs[res.job.id] = res.job
    toast('ok', `${r.file} restored`)
    selected.value = null
    await load()
  } catch (e: any) {
    toast('bad', e.message)
  }
}
</script>

<template>
  <div class="grid grid-cols-1 gap-4 lg:grid-cols-[minmax(0,20rem)_minmax(0,1fr)]">
    <div class="card self-start">
      <div class="card-h">
        <span>Versions <span class="font-normal text-zinc-500">({{ shown.length }})</span></span>
        <select v-if="fileNames.length > 1" v-model="fileFilter" class="input !w-auto !py-1 text-xs font-normal" aria-label="File">
          <option value="">all files</option>
          <option v-for="f in fileNames" :key="f" :value="f">{{ f }}</option>
        </select>
      </div>
      <div v-if="!shown.length" class="muted px-4 py-6 text-center text-sm">No saved versions yet. They appear when files are opened, edited or changed elsewhere.</div>
      <ul class="max-h-[65vh] divide-y divide-zinc-100 overflow-auto dark:divide-zinc-800/70">
        <li v-for="r in shown" :key="r.id">
          <button
            class="w-full px-4 py-2 text-left text-sm hover:bg-zinc-50 dark:hover:bg-zinc-800/30"
            :class="selected?.id === r.id ? 'bg-emerald-50/60 dark:bg-emerald-950/30' : ''"
            @click="pick(r)"
          >
            <div class="flex items-center gap-2">
              <span class="font-mono text-xs">{{ r.file }}</span>
              <span class="badge !rounded-md !px-1.5 !py-0" :class="reasonClass[r.reason]">{{ reasonText[r.reason] ?? r.reason }}</span>
            </div>
            <div class="muted mt-0.5 text-xs" :title="dateTime(r.created_at)">{{ ago(r.created_at) }}<template v-if="r.ip"> · {{ r.ip }}</template></div>
          </button>
        </li>
      </ul>
    </div>

    <div v-if="selected" class="card self-start overflow-hidden">
      <div class="card-h flex-wrap">
        <span class="font-mono text-sm">{{ selected.file }} · {{ dateTime(selected.created_at) }}</span>
        <div class="flex flex-wrap items-center gap-1.5 font-normal">
          <select v-model="against" class="input !w-auto !py-1 text-xs" aria-label="Compare with">
            <option value="current">compared with the current file</option>
            <option value="previous">compared with the version before</option>
          </select>
          <button class="btn btn-sm" :disabled="state.info?.read_only" @click="restore(false)">Restore</button>
          <button class="btn btn-sm btn-primary" :disabled="state.info?.read_only || stack.self" @click="restore(true)">Restore &amp; apply</button>
        </div>
      </div>
      <div class="muted border-b border-zinc-200 px-4 py-1.5 text-xs dark:border-zinc-800">
        <template v-if="against === 'current'">Green: what restoring would bring back. Red: what it would remove.</template>
        <template v-else>Changes this version made.</template>
      </div>
      <div v-if="error" class="px-4 py-3 text-sm text-red-600 dark:text-red-400">{{ error }}</div>
      <div v-else-if="loading" class="muted px-4 py-6 text-center text-sm">Loading…</div>
      <DiffView v-else :original="left" :modified="right" :lang="langFor(selected.file)" height="60vh" />
    </div>
  </div>
</template>
