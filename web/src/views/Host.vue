<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api, type HostInfo, type HostSample, type PrunePreview } from '../api'
import { bytes, duration, gib, shortImage } from '../format'
import { ask, state, toast } from '../state'
import Meter from '../components/Meter.vue'
import TimeChart from '../components/TimeChart.vue'

const info = ref<HostInfo | null>(null)
const samples = ref<HostSample[]>([])
const prune = ref<PrunePreview | null>(null)

async function load() {
  try {
    const [h, hist] = await Promise.all([api.host(), api.hostHistory()])
    info.value = h
    samples.value = hist.samples
  } catch (e: any) {
    toast('bad', e.message)
  }
}
async function loadPrune() {
  try {
    prune.value = await api.prunePreview()
  } catch (e: any) {
    toast('bad', e.message)
  }
}
onMounted(() => {
  load()
  loadPrune()
})
const timer = window.setInterval(() => api.hostHistory().then((h) => (samples.value = h.samples)).catch(() => {}), 10_000)
onBeforeUnmount(() => window.clearInterval(timer))
// refresh the cleanup preview when a prune job finishes
watch(() => Object.values(state.jobs).filter((j) => j.kind === 'prune' && j.state === 'succeeded').length, () => {
  loadPrune()
  load()
})

const host = computed(() => state.host ?? info.value?.host)
const cpu = computed(() => samples.value.map((s) => ({ t: Date.parse(s.t), v: s.cpu })))
const mem = computed(() => samples.value.map((s) => ({ t: Date.parse(s.t), v: s.mem })))
const memMax = computed(() => samples.value[0]?.mem_total ?? host.value?.mem_total ?? 1)

// containers by memory, with their stack
const top = computed(() => {
  const out: { name: string; stack: string; cpu: number; mem: number }[] = []
  for (const st of state.stacks)
    for (const sv of st.services)
      for (const c of sv.containers) {
        const s = state.cstats[c.id]
        if (s) out.push({ name: c.name, stack: st.id, cpu: s.cpu_pct, mem: s.mem_bytes })
      }
  return out.sort((a, b) => b.mem - a.mem).slice(0, 12)
})

async function runPrune(what: 'dangling' | 'unused' | 'build_cache') {
  const p = prune.value
  const texts = {
    dangling: { title: 'Remove dangling images?', text: `${p?.dangling.length ?? 0} untagged images, about ${bytes(p?.dangling_size)}.` },
    unused: {
      title: 'Remove unused images?',
      text: `${p?.unused.length ?? 0} tagged images no container or compose file uses, about ${bytes(p?.unused_size)}. They'd be downloaded again if needed. Rollback images are kept.`,
    },
    build_cache: { title: 'Clear the build cache?', text: `About ${bytes(p?.build_cache_reclaimable)}. The next build of a stack takes longer.` },
  }[what]
  if (!(await ask({ ...texts, confirm: 'Remove', danger: what === 'unused' }))) return
  try {
    const j = await api.prune(what)
    state.jobs[j.id] = j
  } catch (e: any) {
    toast('bad', e.message)
  }
}
const showUnused = ref(false)
</script>

<template>
  <div class="space-y-4">
    <div class="grid grid-cols-1 gap-4 lg:grid-cols-3">
      <div class="card p-4 lg:col-span-2">
        <div class="grid grid-cols-1 gap-6 sm:grid-cols-2">
          <TimeChart title="CPU (30 min)" :points="cpu" :max="100" unit="%" color="var(--series-1)" />
          <TimeChart title="Memory (30 min)" :points="mem" :max="memMax" :format="(v) => gib(v) + ' GiB'" color="var(--series-3)" />
        </div>
      </div>
      <div class="card p-4 text-sm" v-if="host">
        <div class="text-xs font-medium uppercase tracking-wide text-zinc-500">Host</div>
        <div class="font-semibold">{{ info?.system.name || state.info?.hostname }}</div>
        <div class="muted text-xs">{{ info?.system.os }} · kernel {{ info?.system.kernel }}</div>
        <dl class="mt-3 grid grid-cols-2 gap-x-3 gap-y-1 text-xs tabular-nums">
          <dt class="text-zinc-500">CPU</dt><dd class="text-right">{{ host.cores }} cores · {{ Math.round(host.cpu_pct) }}%</dd>
          <dt class="text-zinc-500">load</dt><dd class="text-right">{{ host.load.map((l) => l.toFixed(2)).join(' / ') }}</dd>
          <dt class="text-zinc-500">memory</dt><dd class="text-right">{{ gib(host.mem_used) }} / {{ gib(host.mem_total) }} GiB</dd>
          <dt class="text-zinc-500">swap</dt><dd class="text-right">{{ gib(host.swap_used) }} / {{ gib(host.swap_total) }} GiB</dd>
          <dt class="text-zinc-500">uptime</dt><dd class="text-right">{{ duration(host.uptime_sec) }}</dd>
          <dt class="text-zinc-500">docker</dt><dd class="text-right">{{ info?.system.version }} · compose {{ info?.compose_version }}</dd>
          <dt class="text-zinc-500">storage</dt><dd class="text-right">{{ info?.system.storage_driver }}</dd>
          <dt class="text-zinc-500">containers</dt><dd class="text-right">{{ info?.system.running }} running / {{ info?.system.containers }}</dd>
        </dl>
        <div class="mt-3">
          <div class="mb-1 flex justify-between text-xs tabular-nums"><span class="text-zinc-500">disk {{ host.disk_path }}</span><span>{{ gib(host.disk_used) }} / {{ gib(host.disk_total) }} GiB</span></div>
          <Meter :value="host.disk_used" :max="host.disk_total" />
        </div>
      </div>
    </div>

    <div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <div class="card self-start">
        <div class="card-h">Containers by memory</div>
        <div v-if="!top.length" class="muted px-4 py-6 text-center text-sm">No stats yet.</div>
        <table v-else class="w-full table-fixed text-sm">
          <tbody>
            <tr v-for="c in top" :key="c.name" class="border-b border-zinc-100 last:border-0 dark:border-zinc-800/70">
              <td class="max-w-[12rem] truncate px-4 py-1.5 font-mono text-xs whitespace-nowrap" :title="c.name">{{ c.name }}</td>
              <td class="max-w-[10rem] truncate px-2 py-1.5 text-xs whitespace-nowrap text-zinc-500" :title="c.stack">{{ c.stack }}</td>
              <td class="w-32 px-2 py-1.5"><Meter :value="c.mem" :max="host?.mem_total ?? 1" /></td>
              <td class="px-2 py-1.5 text-right text-xs whitespace-nowrap tabular-nums">{{ bytes(c.mem, 0) }}</td>
              <td class="px-4 py-1.5 text-right text-xs text-zinc-500 tabular-nums">{{ c.cpu.toFixed(1) }}%</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="card self-start">
        <div class="card-h">Clean up <button class="btn btn-sm font-normal" @click="loadPrune">Refresh</button></div>
        <div v-if="!prune" class="muted px-4 py-6 text-center text-sm">Looking…</div>
        <div v-else class="divide-y divide-zinc-100 text-sm dark:divide-zinc-800/70">
          <div class="flex items-center gap-3 px-4 py-3">
            <div class="min-w-0 flex-1">
              <div class="font-medium">Dangling images</div>
              <div class="muted text-xs">{{ prune.dangling.length }} untagged leftovers of updates and builds · {{ bytes(prune.dangling_size) }}</div>
            </div>
            <button class="btn btn-sm" :disabled="!prune.dangling.length || state.info?.read_only" @click="runPrune('dangling')">Remove</button>
          </div>
          <div class="px-4 py-3">
            <div class="flex items-center gap-3">
              <div class="min-w-0 flex-1">
                <div class="font-medium">Unused images</div>
                <div class="muted text-xs">
                  {{ prune.unused.length }} images no container or compose file uses · {{ bytes(prune.unused_size) }}
                  <button v-if="prune.unused.length" class="text-emerald-700 hover:underline dark:text-emerald-400" @click="showUnused = !showUnused">{{ showUnused ? 'hide' : 'show' }}</button>
                </div>
              </div>
              <button class="btn btn-sm" :disabled="!prune.unused.length || state.info?.read_only" @click="runPrune('unused')">Remove</button>
            </div>
            <ul v-if="showUnused" class="mt-2 max-h-48 space-y-0.5 overflow-auto font-mono text-xs">
              <li v-for="i in prune.unused" :key="i.id" class="flex justify-between gap-2"><span class="truncate">{{ i.tags.map(shortImage).join(', ') }}</span><span class="muted shrink-0">{{ bytes(i.size) }}</span></li>
            </ul>
          </div>
          <div class="flex items-center gap-3 px-4 py-3">
            <div class="min-w-0 flex-1">
              <div class="font-medium">Build cache</div>
              <div class="muted text-xs">{{ bytes(prune.build_cache_size) }}, {{ bytes(prune.build_cache_reclaimable) }} reclaimable</div>
            </div>
            <button class="btn btn-sm" :disabled="!prune.build_cache_reclaimable || state.info?.read_only" @click="runPrune('build_cache')">Clear</button>
          </div>
          <div class="muted px-4 py-2.5 text-xs">
            Kept: {{ prune.rollback_images }} rollback images ({{ bytes(prune.rollback_size) }}), {{ prune.protected_images }} images named in compose files, and Dockira itself.
            Volumes are never touched here.
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
