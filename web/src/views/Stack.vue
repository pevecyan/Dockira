<script setup lang="ts">
import { computed, defineAsyncComponent, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, type ContainerDetails, type Job, type StackAction } from '../api'
import { ago, plural, shortImage, since } from '../format'
import { activeJobs, jobFor, runAction, stackUsage, state, toast } from '../state'
import { bytes, pct } from '../format'
import Icon from '../components/Icon.vue'
import JobOutput from '../components/JobOutput.vue'
import LogPanel from '../components/LogPanel.vue'
import Menu, { type Item } from '../components/Menu.vue'
import StatusBadge from '../components/StatusBadge.vue'
import FeedPanel from '../components/FeedPanel.vue'
import StackUpdates from '../components/StackUpdates.vue'

// the editor pulls in CodeMirror: load it only when a file tab opens
const ComposeEditor = defineAsyncComponent(() => import('../components/ComposeEditor.vue'))
const History = defineAsyncComponent(() => import('../components/History.vue'))
const Terminal = defineAsyncComponent(() => import('../components/Terminal.vue'))
const FilesBrowser = defineAsyncComponent(() => import('../components/FilesBrowser.vue'))
const Backups = defineAsyncComponent(() => import('../components/Backups.vue'))
const shell = ref<{ id: string; name: string } | null>(null)

const route = useRoute()
const router = useRouter()
const id = computed(() => String(route.params.id))
const stack = computed(() => state.stacks.find((s) => s.id === id.value))

const tabs = computed(() => {
  const t = [
    { key: 'overview', label: 'Overview' },
    { key: 'logs', label: 'Logs' },
  ]
  if (stack.value && !stack.value.orphaned) {
    t.push({ key: 'compose', label: 'Compose' })
    t.push({ key: 'files', label: 'Files' })
    if (!stack.value.self) t.push({ key: 'backups', label: 'Backups' })
    t.push({ key: 'history', label: 'History' })
    t.push({ key: 'updates', label: 'Updates' })
  }
  t.push({ key: 'jobs', label: 'Jobs' })
  return t
})
// old links to the .env tab open .env in the Files tab
if (route.query.tab === 'env') router.replace({ query: { tab: 'files', file: '.env' } })

const tab = computed({
  get: () => (tabs.value.some((t) => t.key === route.query.tab) ? String(route.query.tab) : 'overview'),
  set: (v: string) => router.replace({ query: { tab: v === 'overview' ? undefined : v } }),
})

// inspect details (start time, restarts, mounts), refreshed when the stack's containers change
const details = ref<Record<string, ContainerDetails>>({})
const loadError = ref('')
async function loadDetails() {
  try {
    const d = await api.stack(id.value)
    details.value = Object.fromEntries(d.details.map((x) => [x.id, x]))
    loadError.value = ''
  } catch (e: any) {
    loadError.value = e.message
  }
}
const fingerprint = computed(() => stack.value?.services.flatMap((s) => s.containers.map((c) => c.id + c.state + c.health)).join() ?? '')
let timer: number | undefined
watch([id, fingerprint], () => {
  window.clearTimeout(timer)
  timer = window.setTimeout(loadDetails, 300)
}, { immediate: true })

const job = computed(() => (stack.value ? jobFor(stack.value.id) : undefined))
const busy = computed(() => !!job.value || !!state.info?.read_only)
const run = (a: StackAction, services?: string[]) => stack.value && runAction(stack.value, a, services)

const primary = computed(() => {
  const st = stack.value
  if (!st || st.status === 'invalid') return []
  const b: { label: string; action: StackAction; cls?: string; icon: string }[] = []
  if (st.status === 'down') return st.orphaned ? [] : [{ label: 'Start', action: 'up' as StackAction, cls: 'btn-primary', icon: 'play' }]
  if (st.running === 0) b.push({ label: 'Start', action: 'start', cls: 'btn-primary', icon: 'play' })
  else b.push({ label: 'Restart', action: 'restart', icon: 'restart' })
  if (!st.orphaned) {
    b.push({ label: 'Pull', action: 'pull', icon: 'download' })
    b.push({ label: 'Update', action: 'update', icon: 'updates' })
  }
  if (st.running > 0 && !st.self) b.push({ label: 'Stop', action: 'stop', cls: '!text-red-600 dark:!text-red-400', icon: 'stop' })
  return b
})

const more = computed<Item[]>(() => {
  const st = stack.value
  if (!st) return []
  const items: Item[] = []
  if (!st.orphaned) {
    items.push({ label: 'Up (apply compose file)', run: () => run('up'), disabled: busy.value })
    items.push({ label: 'Recreate containers', run: () => run('recreate'), disabled: busy.value })
    if (st.services.some((s) => s.build)) items.push({ label: 'Rebuild & restart', run: () => run('rebuild'), disabled: busy.value })
  }
  if (!st.self) {
    items.push({ label: 'Take down', danger: true, run: () => run('down'), disabled: busy.value || st.total === 0 })
    if (!st.orphaned) items.push({ label: 'Take down + delete volumes', danger: true, run: () => run('down_volumes'), disabled: busy.value || st.total === 0 })
  }
  return items
})

async function containerAction(cid: string, name: string, action: 'start' | 'stop' | 'restart') {
  try {
    await api.containerAction(cid, action)
    toast('ok', `${action === 'stop' ? 'Stopped' : action === 'start' ? 'Started' : 'Restarted'} ${name}`)
  } catch (e: any) {
    toast('bad', e.message)
  }
}

const usage = computed(() => (stack.value ? stackUsage(stack.value) : { cpu: 0, mem: 0 }))
const host = window.location.hostname

// jobs tab
const jobs = ref<Job[]>([])
const openJob = ref<number | null>(null)
async function loadJobs() {
  try {
    jobs.value = (await api.jobs({ stack: id.value, limit: 50 })).jobs
  } catch (e: any) {
    toast('bad', e.message)
  }
}
watch([tab, () => activeJobs().length], () => tab.value === 'jobs' && loadJobs(), { immediate: true })

const logContainer = ref<string | undefined>(undefined)
function showLogs(cid?: string) {
  logContainer.value = cid
  tab.value = 'logs'
}
</script>

<template>
  <div v-if="!stack" class="card px-4 py-10 text-center text-sm">
    <template v-if="state.stacksLoaded">
      <p class="font-medium">No stack “{{ id }}”.</p>
      <p class="muted mt-1">It may have been moved or deleted. <RouterLink to="/" class="text-emerald-700 hover:underline dark:text-emerald-400">Back to the dashboard</RouterLink></p>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>

  <div v-else class="space-y-4">
    <!-- header -->
    <div class="card p-4">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0">
          <div class="text-xs font-medium uppercase tracking-wide text-zinc-500">Stack</div>
          <div class="mt-0.5 flex flex-wrap items-center gap-2">
            <h1 class="text-xl font-semibold break-all">{{ stack.id.startsWith('~') ? stack.name : stack.id }}</h1>
            <StatusBadge v-if="job" status="running_job" :text="job.kind.replace('_', ' ') + '…'" />
            <StatusBadge v-else :status="stack.status" />
            <span v-if="stack.self" class="badge bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300">this app</span>
            <span v-if="stack.external" class="badge bg-sky-100 text-sky-800 dark:bg-sky-950 dark:text-sky-300">external</span>
            <span v-else-if="stack.orphaned" class="badge bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300">orphaned</span>
          </div>
          <div class="muted mt-1 text-xs break-all">
            <span class="font-mono">{{ stack.dir || '–' }}</span>
            <template v-if="stack.files.length"> · {{ stack.files.map((f) => f.split('/').pop()).join(' + ') }}</template>
            <template v-if="stack.env_file"> · .env</template>
            · project <span class="font-mono">{{ stack.name }}</span>
          </div>
          <div v-if="stack.running" class="muted mt-1 text-xs tabular-nums">{{ stack.running }}/{{ stack.total }} running · CPU {{ pct(usage.cpu) }} · RAM {{ bytes(usage.mem) }}</div>
        </div>
        <div class="flex flex-wrap items-center gap-1.5">
          <button v-for="b in primary" :key="b.label" class="btn btn-sm" :class="b.cls" :disabled="busy" @click="run(b.action)">
            <Icon :name="b.icon" :size="14" />{{ b.label }}
          </button>
          <button v-if="job" class="btn btn-sm" @click="state.jobPanel = job.id">Show output</button>
          <Menu v-if="more.length" :items="more" />
        </div>
      </div>
      <div v-if="stack.error" class="mt-3 rounded-lg border px-3 py-2 text-sm whitespace-pre-wrap" :class="stack.status === 'invalid' ? 'border-red-300 bg-red-50 text-red-800 dark:border-red-900 dark:bg-red-950/50 dark:text-red-200' : 'border-zinc-300 bg-zinc-50 text-zinc-700 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-300'">
        {{ stack.error }}
      </div>
    </div>

    <!-- tabs -->
    <div class="no-scrollbar flex gap-1 overflow-x-auto overflow-y-hidden shadow-[inset_0_-1px_0] shadow-zinc-200 dark:shadow-zinc-800" role="tablist">
      <button
        v-for="t in tabs"
        :key="t.key"
        role="tab"
        :aria-selected="tab === t.key"
        class="border-b-2 px-3 py-2 text-sm font-medium whitespace-nowrap transition outline-none focus-visible:rounded-t-md focus-visible:bg-zinc-100 dark:focus-visible:bg-zinc-800"
        :class="tab === t.key ? 'border-emerald-500 text-emerald-700 dark:text-emerald-400' : 'border-transparent text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200'"
        @click="tab = t.key"
      >
        {{ t.label }}
      </button>
    </div>

    <!-- overview -->
    <template v-if="tab === 'overview'">
      <div class="card">
        <div class="card-h"><span>Services <span class="font-normal text-zinc-500">({{ stack.services.length }})</span></span></div>
        <div v-if="loadError" class="px-4 py-2 text-sm text-red-600">{{ loadError }}</div>
        <div v-if="!stack.services.length" class="muted px-4 py-6 text-center text-sm">No services.</div>
        <div class="divide-y divide-zinc-100 dark:divide-zinc-800/70">
          <div v-for="sv in stack.services" :key="sv.name" class="px-4 py-3">
            <div class="flex flex-wrap items-start justify-between gap-2">
              <div class="min-w-0">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-medium">{{ sv.name }}</span>
                  <StatusBadge :status="sv.status" />
                  <span v-if="sv.build" class="badge !rounded-md !px-1.5 !py-0 bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400">build</span>
                  <span v-if="sv.stateful" class="badge !rounded-md !px-1.5 !py-0 bg-violet-100 text-violet-700 dark:bg-violet-950 dark:text-violet-300" title="Holds data">db</span>
                  <span v-if="!sv.in_file && !stack.orphaned" class="badge !rounded-md !px-1.5 !py-0 bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300" title="No longer in the compose file">leftover</span>
                  <span v-if="!sv.healthcheck" class="text-[11px] text-zinc-400" title="No healthcheck: updates are verified by 'running without restarts'">no healthcheck</span>
                </div>
                <div class="muted mt-0.5 font-mono text-xs break-all">{{ shortImage(sv.image) }}</div>
              </div>
              <div class="flex flex-wrap gap-1.5" v-if="!stack.orphaned && sv.in_file">
                <button class="btn btn-sm" :disabled="busy" @click="run(sv.containers.some((c) => c.state === 'running') ? 'restart' : 'up', [sv.name])">
                  {{ sv.containers.some((c) => c.state === 'running') ? 'Restart' : 'Start' }}
                </button>
                <button class="btn btn-sm" :disabled="busy" @click="run('update', [sv.name])">Update</button>
                <button class="btn btn-sm" :disabled="busy" @click="run('recreate', [sv.name])">Recreate</button>
                <button v-if="sv.containers.some((c) => c.state === 'running') && !stack.self" class="btn btn-sm !text-red-600 dark:!text-red-400" :disabled="busy" @click="run('stop', [sv.name])">Stop</button>
              </div>
            </div>
            <!-- the service's containers, indented under it with a guide line -->
            <div class="mt-2 ml-1.5 border-l-2 border-zinc-200 pl-4 dark:border-zinc-800">
            <table v-if="sv.containers.length" class="w-full table-fixed text-xs">
              <colgroup>
                <col />
                <col class="w-32" />
                <col class="hidden w-36 sm:table-column" />
                <col class="hidden w-24 md:table-column" />
                <col class="hidden w-32 md:table-column" />
                <col class="hidden w-32 lg:table-column" />
                <col class="w-24" />
              </colgroup>
              <tbody>
                <tr v-for="c in sv.containers" :key="c.id" class="border-b border-zinc-100 last:border-0 dark:border-zinc-800/70">
                  <td class="truncate py-1.5 pr-2 font-mono" :title="c.name">{{ c.name }}</td>
                  <td class="py-1.5 pr-2">
                    <StatusBadge :status="c.health && c.state === 'running' ? c.health : c.state" :exit-code="c.exit_code" :text="c.state === 'exited' ? `exited (${c.exit_code})` : undefined" :title="c.status" />
                  </td>
                  <td class="hidden py-1.5 pr-2 text-zinc-500 sm:table-cell" :title="details[c.id]?.started_at">
                    <template v-if="c.state === 'running'">up {{ since(details[c.id]?.started_at) }}</template>
                    <template v-else-if="details[c.id]?.finished_at">stopped {{ ago(details[c.id]?.finished_at) }}</template>
                  </td>
                  <td class="hidden py-1.5 pr-2 text-zinc-500 md:table-cell">
                    <span v-if="details[c.id]?.restart_count" :class="details[c.id].restart_count > 3 ? 'text-amber-600 dark:text-amber-400' : ''">{{ plural(details[c.id].restart_count, 'restart') }}</span>
                  </td>
                  <td class="hidden truncate py-1.5 pr-2 md:table-cell">
                    <template v-for="p in c.ports ?? []" :key="`${p.public}${p.private}${p.type}`">
                      <a v-if="p.public" :href="`http://${host}:${p.public}`" target="_blank" rel="noopener" class="mr-2 text-sky-700 hover:underline dark:text-sky-400">{{ p.public }}→{{ p.private }}</a>
                    </template>
                  </td>
                  <td class="hidden py-1.5 pr-2 text-right text-zinc-500 tabular-nums lg:table-cell">
                    <template v-if="state.cstats[c.id]">{{ pct(state.cstats[c.id].cpu_pct) }} · {{ bytes(state.cstats[c.id].mem_bytes, 0) }}</template>
                  </td>
                  <td class="py-1.5 text-right whitespace-nowrap">
                    <button class="rounded p-1 text-zinc-500 hover:bg-zinc-100 dark:hover:bg-zinc-800" title="Logs" @click="showLogs(c.id)"><Icon name="logs" :size="15" /></button>
                    <button class="rounded p-1 text-zinc-500 hover:bg-zinc-100 disabled:opacity-40 dark:hover:bg-zinc-800" title="Restart container" :disabled="state.info?.read_only" @click="containerAction(c.id, c.name, 'restart')"><Icon name="restart" :size="15" /></button>
                    <button v-if="state.info?.exec_enabled && c.state === 'running' && !state.info?.read_only" class="rounded p-1 text-zinc-500 hover:bg-zinc-100 dark:hover:bg-zinc-800" title="Open a shell" @click="shell = { id: c.id, name: c.name }"><Icon name="terminal" :size="15" /></button>
                    <button v-if="c.state !== 'running'" class="rounded p-1 text-zinc-500 hover:bg-zinc-100 disabled:opacity-40 dark:hover:bg-zinc-800" title="Start container" :disabled="state.info?.read_only" @click="containerAction(c.id, c.name, 'start')"><Icon name="play" :size="15" /></button>
                  </td>
                </tr>
              </tbody>
            </table>
            <div v-else class="muted py-1.5 text-xs">No container.</div>
            <details v-for="c in sv.containers.filter((c) => details[c.id]?.mounts?.length || details[c.id]?.health_log?.length)" :key="'d' + c.id" class="mt-1 text-xs">
              <summary class="cursor-pointer text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200">Mounts &amp; health of {{ c.name }}</summary>
              <div class="mt-1 space-y-0.5 pl-3 font-mono">
                <div v-for="m in details[c.id].mounts ?? []" :key="m.destination" class="break-all">
                  <span class="text-zinc-500">{{ m.type }}</span> {{ m.name || m.source }} → {{ m.destination }}<span v-if="!m.rw" class="text-zinc-500"> (ro)</span>
                </div>
                <div v-for="(h, i) in details[c.id].health_log ?? []" :key="i" class="break-all text-zinc-500">{{ h }}</div>
              </div>
            </details>
            </div>
          </div>
        </div>
      </div>
      <FeedPanel :stack="stack.id" title="Recent events" height="14rem" />
    </template>

    <div v-else-if="tab === 'logs'" class="space-y-2">
      <LogPanel
        :key="logContainer ?? 'stack'"
        :stack="logContainer ? undefined : stack.id"
        :container="logContainer"
        :services="stack.services.map((s) => s.name)"
        :title="logContainer ? `Logs of ${stack.services.flatMap((s) => s.containers).find((c) => c.id === logContainer)?.name ?? 'container'}` : 'Logs'"
        height="calc(100vh - 22rem)"
      />
      <button v-if="logContainer" class="text-xs text-emerald-700 hover:underline dark:text-emerald-400" @click="logContainer = undefined">Show the whole stack</button>
    </div>

    <ComposeEditor v-else-if="tab === 'compose'" :key="stack.id + 'compose'" :stack="stack" />
    <FilesBrowser v-else-if="tab === 'files'" :key="stack.id + 'files'" :stack="stack" />
    <Backups v-else-if="tab === 'backups'" :key="stack.id + 'backups'" :stack="stack" />
    <History v-else-if="tab === 'history'" :stack="stack" />
    <StackUpdates v-else-if="tab === 'updates'" :stack="stack" />

    <template v-else-if="tab === 'jobs'">
      <div class="card">
        <div class="card-h">Jobs <button class="btn btn-sm font-normal" @click="loadJobs">Reload</button></div>
        <div v-if="!jobs.length" class="muted px-4 py-6 text-center text-sm">No jobs for this stack yet.</div>
        <ul class="divide-y divide-zinc-100 dark:divide-zinc-800/70">
          <li v-for="j in jobs" :key="j.id">
            <button class="flex w-full items-center gap-3 px-4 py-2 text-left text-sm hover:bg-zinc-50 dark:hover:bg-zinc-800/30" @click="openJob = openJob === j.id ? null : j.id">
              <StatusBadge :status="j.state === 'running' ? 'running_job' : j.state" :text="j.state" />
              <span class="min-w-0 flex-1 truncate">{{ j.title }}</span>
              <span class="muted text-xs whitespace-nowrap">{{ j.trigger !== 'user' ? j.trigger + ' · ' : '' }}{{ ago(j.created_at) }}</span>
            </button>
            <JobOutput v-if="openJob === j.id" :id="j.id" height="20rem" />
          </li>
        </ul>
      </div>
    </template>
    <Terminal v-if="shell" :container="shell.id" :name="shell.name" @close="shell = null" />
  </div>
</template>
