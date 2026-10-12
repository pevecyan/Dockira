<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api, urls, type Backups, type Stack } from '../api'
import { ago, bytes, dateTime } from '../format'
import { activeJobs, ask, confirmPassword, jobFor, state, toast } from '../state'

// A stack's volume snapshots (take, restore, download, delete) and its database dumps.
const props = defineProps<{ stack: Stack }>()
const data = ref<Backups | null>(null)
const stop = ref(false)
const binds = ref(true)
const open = ref<string | null>(null)

async function load() {
  try {
    data.value = await api.backups(props.stack.id)
  } catch (e: any) {
    toast('bad', e.message)
  }
}
load()
// reload when a snapshot or restore job of this stack finishes
watch(() => activeJobs().filter((j) => j.stack_id === props.stack.id).length, (n, old) => n < old && load())

const busy = computed(() => !!jobFor(props.stack.id) || !!state.info?.read_only)
const hasData = computed(() => !!data.value && (data.value.volumes > 0 || data.value.binds > 0))

async function take() {
  try {
    const j = await api.takeSnapshot(props.stack.id, { stop: stop.value, binds: binds.value })
    state.jobs[j.id] = j
  } catch (e: any) {
    toast('bad', e.message)
  }
}

async function restore(id: string) {
  const ok = await ask({
    title: `Restore the snapshot from ${dateTime(snapTime(id))}?`,
    text: `${props.stack.name} is stopped, its current data is saved as a "before restore" snapshot, the volumes are replaced with this snapshot, and it starts again.`,
    confirm: 'Restore',
    danger: true,
  })
  if (!ok) return
  try {
    const j = await api.restoreSnapshot(props.stack.id, id)
    state.jobs[j.id] = j
    state.jobPanel = j.id
  } catch (e: any) {
    toast('bad', e.message)
  }
}

async function remove(id: string) {
  if (!(await ask({ title: 'Delete this snapshot?', text: 'Its archives are deleted for good.', confirm: 'Delete', danger: true }))) return
  try {
    await api.deleteSnapshot(props.stack.id, id)
    load()
  } catch (e: any) {
    toast('bad', e.message)
  }
}

async function download(p: { snapshot?: string; file?: string; dump?: string }) {
  if (!(await confirmPassword())) return
  const a = document.createElement('a')
  a.href = urls.backup(props.stack.id, p)
  a.click()
}

const snapTime = (id: string) => data.value?.snapshots.find((s) => s.id === id)?.created ?? ''
const triggerClass: Record<string, string> = {
  manual: 'bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300',
  update: 'bg-sky-100 text-sky-800 dark:bg-sky-950 dark:text-sky-300',
  'before restore': 'bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300',
}
</script>

<template>
  <div v-if="!data" class="card muted px-4 py-8 text-center text-sm">Loading…</div>
  <div v-else class="space-y-4">
    <div class="card">
      <div class="card-h">
        <span>Volume snapshots <span class="font-normal text-zinc-500">({{ data.snapshots.length }})</span></span>
        <button class="btn btn-sm btn-primary font-normal" :disabled="busy || !hasData || stack.self" @click="take">Snapshot now</button>
      </div>
      <div class="flex flex-wrap items-center gap-x-4 gap-y-1.5 border-b border-zinc-200 px-4 py-2 text-xs dark:border-zinc-800">
        <span class="muted">{{ data.volumes }} volume{{ data.volumes === 1 ? '' : 's' }} · {{ data.binds }} bind folder{{ data.binds === 1 ? '' : 's' }}</span>
        <label class="flex items-center gap-1.5" title="Copy bind-mounted folders inside the app's folder too (like ./data)"><input v-model="binds" type="checkbox" class="accent-emerald-600" />include bind folders</label>
        <label class="flex items-center gap-1.5" title="Databases are consistent when nothing writes while copying"><input v-model="stop" type="checkbox" class="accent-emerald-600" />stop the app while copying</label>
        <span class="muted ml-auto">Before every update too: add a “volume snapshot” step in Updates → Update policy.</span>
      </div>
      <div v-if="stack.self" class="muted px-4 py-6 text-center text-sm">Dockira's own data lives in its data folder on the host; back that folder up together with config.yaml.</div>
      <div v-else-if="!hasData" class="muted px-4 py-6 text-center text-sm">This app has no volumes or bind folders inside its folder: nothing to snapshot.</div>
      <div v-else-if="!data.snapshots.length" class="muted px-4 py-6 text-center text-sm">No snapshots yet.</div>
      <ul class="divide-y divide-zinc-100 text-sm dark:divide-zinc-800/70">
        <li v-for="s in data.snapshots" :key="s.id">
          <div class="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5">
            <button class="flex min-w-0 items-center gap-2 text-left" @click="open = open === s.id ? null : s.id">
              <span class="text-zinc-400">{{ open === s.id ? '▾' : '▸' }}</span>
              <span class="font-medium" :title="dateTime(s.created)">{{ dateTime(s.created) }}</span>
            </button>
            <span class="badge" :class="triggerClass[s.trigger] ?? triggerClass.manual">{{ s.trigger }}</span>
            <span class="muted text-xs">{{ ago(s.created) }} · {{ s.items.length }} item{{ s.items.length === 1 ? '' : 's' }} · {{ bytes(s.size) }}<template v-if="s.stopped"> · taken while stopped</template></span>
            <div class="ml-auto flex gap-1.5">
              <button class="btn btn-sm" :disabled="busy" @click="restore(s.id)">Restore</button>
              <button class="btn btn-sm !text-red-600 dark:!text-red-400" :disabled="busy" @click="remove(s.id)">Delete</button>
            </div>
          </div>
          <ul v-if="open === s.id" class="mx-4 mb-3 ml-1.5 space-y-1 border-l-2 border-zinc-200 pl-4 text-xs dark:border-zinc-800">
            <li v-for="it in s.items" :key="it.file" class="flex flex-wrap items-center gap-x-3">
              <span class="badge bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400">{{ it.kind }}</span>
              <span class="font-mono">{{ it.name }}</span>
              <span class="muted">→ {{ it.target }}</span>
              <span class="muted tabular-nums">{{ bytes(it.size) }}</span>
              <button class="ml-auto text-emerald-700 hover:underline dark:text-emerald-400" @click="download({ snapshot: s.id, file: it.file })">download</button>
            </li>
          </ul>
        </li>
      </ul>
    </div>

    <div class="card">
      <div class="card-h"><span>Database dumps <span class="font-normal text-zinc-500">({{ data.dumps.length }})</span></span></div>
      <div v-if="!data.dumps.length" class="muted px-4 py-6 text-center text-sm">
        None yet. Add a “pg_dump backup” step in Updates → Update policy to dump a postgres service before every update.
      </div>
      <ul class="divide-y divide-zinc-100 text-sm dark:divide-zinc-800/70">
        <li v-for="d in data.dumps" :key="d.file" class="flex flex-wrap items-center gap-x-3 px-4 py-2">
          <span class="font-medium">{{ d.service }}</span>
          <span class="muted text-xs" :title="dateTime(d.created)">{{ ago(d.created) }} · {{ bytes(d.size) }}</span>
          <button class="btn btn-sm ml-auto" @click="download({ dump: d.file })">Download</button>
        </li>
      </ul>
    </div>
  </div>
</template>
