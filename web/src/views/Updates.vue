<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, type ServiceUpdate, type UpdateRun, type UpdateSettings } from '../api'
import { ago, dateTime, enc, shortDigest, shortImage } from '../format'
import { hasUpdate, jobFor, loadUpdates, runAction, state, toast } from '../state'
import StatusBadge from '../components/StatusBadge.vue'

const runs = ref<UpdateRun[]>([])
const settings = ref<UpdateSettings | null>(null)
const saved = ref('')
const selected = ref<Set<string>>(new Set())
const filter = ref<'all' | 'updates' | 'problems'>('all')

async function load() {
  try {
    const u = await api.updates()
    state.updates = u.summary
    state.serviceUpdates = u.services
    runs.value = u.runs
  } catch (e: any) {
    toast('bad', e.message)
  }
}
onMounted(async () => {
  load()
  settings.value = await api.updateSettings()
  saved.value = JSON.stringify(settings.value)
})
watch(() => state.updates, () => api.updates().then((u) => (runs.value = u.runs)).catch(() => {}))

const key = (u: ServiceUpdate) => `${u.stack_id}\u0000${u.service}`
const rows = computed(() =>
  state.serviceUpdates.filter((u) => (filter.value === 'updates' ? hasUpdate(u) : filter.value === 'problems' ? u.state === 'unknown' : true)),
)
const selectable = computed(() => rows.value.filter(hasUpdate))
function toggleAll(on: boolean) {
  selected.value = new Set(on ? selectable.value.map(key) : [])
}

async function updateSelected() {
  const byStack = new Map<string, string[]>()
  for (const k of selected.value) {
    const [stack, svc] = k.split('\u0000')
    byStack.set(stack, [...(byStack.get(stack) ?? []), svc])
  }
  for (const [id, svcs] of byStack) {
    const st = state.stacks.find((s) => s.id === id)
    if (st) await runAction(st, 'update', svcs)
  }
  selected.value = new Set()
}

async function updateOne(u: ServiceUpdate) {
  const st = state.stacks.find((s) => s.id === u.stack_id)
  if (st) await runAction(st, 'update', [u.service])
}

async function check() {
  try {
    const j = await api.checkUpdates()
    state.jobs[j.id] = j
  } catch (e: any) {
    toast('bad', e.message)
  }
}

const dirty = computed(() => settings.value && JSON.stringify(settings.value) !== saved.value)
async function saveSettings() {
  if (!settings.value) return
  try {
    settings.value = await api.saveUpdateSettings(settings.value)
    saved.value = JSON.stringify(settings.value)
    toast('ok', 'Update settings saved')
    loadUpdates()
  } catch (e: any) {
    toast('bad', e.message)
  }
}
async function togglePause() {
  if (!settings.value) return
  settings.value.paused = !settings.value.paused
  await saveSettings()
}

const stateText: Record<string, string> = {
  up_to_date: 'up to date', update_available: 'update available', pulled_not_applied: 'pulled, not applied', local_build: 'built locally',
  base_updated: 'base image changed', pinned_digest: 'pinned', unknown: 'check failed', not_checked: 'not checked',
}
const runClass: Record<string, string> = { updated: 'ok', unchanged: 'idle', failed: 'failed', rolled_back: 'rolled_back', running: 'running_job' }
const checkPresets = [
  { label: 'every hour', v: '0 * * * *' },
  { label: 'every 6 hours', v: '0 */6 * * *' },
  { label: 'twice a day', v: '0 6,18 * * *' },
  { label: 'daily at 03:00', v: '0 3 * * *' },
]
const windowPresets = [
  { label: 'daily 04:00', v: '0 4 * * *' },
  { label: 'daily 02:00', v: '0 2 * * *' },
  { label: 'Sundays 04:00', v: '0 4 * * 0' },
  { label: 'weekdays 05:00', v: '0 5 * * 1-5' },
]
</script>

<template>
  <div class="space-y-4">
    <div class="card">
      <div class="card-h flex-wrap">
        <span>
          Updates
          <span class="font-normal text-zinc-500">
            <template v-if="state.updates?.checked">· {{ state.updates.available }} available · checked {{ ago(state.updates.last_check) }}</template>
            <template v-else>· not checked yet</template>
          </span>
        </span>
        <div class="flex flex-wrap items-center gap-2 font-normal">
          <select v-model="filter" class="input !w-auto !py-1 text-xs" aria-label="Show">
            <option value="all">all services</option>
            <option value="updates">with updates</option>
            <option value="problems">check failed</option>
          </select>
          <button class="btn btn-sm" :disabled="state.updates?.checking" @click="check">{{ state.updates?.checking ? 'Checking…' : 'Check now' }}</button>
          <button class="btn btn-sm btn-primary" :disabled="!selected.size || state.info?.read_only" @click="updateSelected">Update selected ({{ selected.size }})</button>
        </div>
      </div>
      <div v-if="!rows.length" class="muted px-4 py-8 text-center text-sm">
        {{ state.serviceUpdates.length ? 'Nothing to show with this filter.' : 'No services yet.' }}
      </div>
      <div class="relative overflow-x-auto">
        <table v-if="rows.length" class="w-full min-w-[56rem] text-sm">
          <thead>
            <tr class="border-b border-zinc-200 text-left text-xs text-zinc-500 dark:border-zinc-800">
              <th class="w-8 px-4 py-2">
                <input type="checkbox" class="accent-emerald-600" :checked="selectable.length > 0 && selected.size === selectable.length" :disabled="!selectable.length" aria-label="Select all with updates" @change="toggleAll(($event.target as HTMLInputElement).checked)" />
              </th>
              <th class="px-2 py-2 font-medium">Service</th>
              <th class="px-2 py-2 font-medium">Image</th>
              <th class="px-2 py-2 font-medium">State</th>
              <th class="px-2 py-2 font-medium">Policy</th>
              <th class="px-2 py-2 font-medium">Automatic</th>
              <th class="px-4 py-2"><span class="sr-only">Action</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="u in rows" :key="key(u)" class="border-b border-zinc-100 align-top last:border-0 dark:border-zinc-800/70">
              <td class="px-4 py-2.5">
                <input
                  v-if="hasUpdate(u)"
                  type="checkbox"
                  class="accent-emerald-600"
                  :checked="selected.has(key(u))"
                  :aria-label="`Select ${u.stack_id}/${u.service}`"
                  @change="($event.target as HTMLInputElement).checked ? selected.add(key(u)) : selected.delete(key(u)); selected = new Set(selected)"
                />
              </td>
              <td class="px-2 py-2.5">
                <RouterLink :to="`/stacks/${enc(u.stack_id)}?tab=updates`" class="font-medium hover:underline">{{ u.stack_id }}</RouterLink>
                <span class="text-zinc-400"> / </span>{{ u.service }}
                <span v-if="u.stateful" class="badge ml-1 !rounded-md !px-1.5 !py-0 bg-violet-100 text-violet-700 dark:bg-violet-950 dark:text-violet-300">db</span>
              </td>
              <td class="max-w-[18rem] px-2 py-2.5 text-xs">
                <div class="truncate font-mono" :title="u.image">{{ u.build ? 'built: ' + u.image : shortImage(u.image) }}</div>
                <div v-for="b in u.base ?? []" :key="b.image" class="muted truncate font-mono">FROM {{ shortImage(b.image) }}<span v-if="b.state === 'base_updated'" class="text-sky-600 dark:text-sky-400"> (changed)</span></div>
                <div v-if="u.hint_same || u.hint_major" class="mt-0.5 text-[11px] text-zinc-500">
                  newer tags: <span v-if="u.hint_same" class="font-mono">{{ u.hint_same }}</span><template v-if="u.hint_same && u.hint_major">, </template><span v-if="u.hint_major" class="font-mono">{{ u.hint_major }}</span> (edit the compose file to move)
                </div>
              </td>
              <td class="px-2 py-2.5 text-xs">
                <StatusBadge :status="u.state" :text="stateText[u.state] ?? u.state" :title="u.error" />
                <div v-if="u.state === 'update_available' && u.local_digest" class="muted mt-1 font-mono text-[11px]">{{ shortDigest(u.local_digest) }} → {{ shortDigest(u.remote_digest) }}</div>
                <div v-if="u.error" class="mt-1 max-w-[16rem] text-[11px] break-words text-amber-700 dark:text-amber-400">{{ u.error_kind ? u.error_kind.replace('_', ' ') + ': ' : '' }}{{ u.error }}</div>
                <div v-if="u.checked_at" class="muted mt-0.5 text-[11px]">checked {{ ago(u.checked_at) }}</div>
              </td>
              <td class="px-2 py-2.5 text-xs">{{ u.mode }}</td>
              <td class="max-w-[16rem] px-2 py-2.5 text-xs">
                <span v-if="u.auto.eligible && !u.auto.blocked" class="text-emerald-700 dark:text-emerald-400">{{ dateTime(u.auto.at) }}</span>
                <span v-else-if="u.auto.blocked" class="text-zinc-500">{{ u.auto.blocked }}</span>
                <span v-else class="text-zinc-400">–</span>
              </td>
              <td class="px-4 py-2.5 text-right">
                <button v-if="hasUpdate(u)" class="btn btn-sm btn-primary" :disabled="!!jobFor(u.stack_id) || state.info?.read_only" @click="updateOne(u)">
                  {{ u.state === 'pulled_not_applied' ? 'Apply' : u.state === 'base_updated' ? 'Rebuild' : 'Update' }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <div class="card self-start">
        <div class="card-h">Recent updates</div>
        <div v-if="!runs.length" class="muted px-4 py-6 text-center text-sm">No updates yet.</div>
        <ul class="max-h-[28rem] divide-y divide-zinc-100 overflow-auto text-sm dark:divide-zinc-800/70">
          <li v-for="r in runs" :key="r.id" class="px-4 py-2">
            <div class="flex flex-wrap items-center gap-2">
              <StatusBadge :status="runClass[r.result] ?? r.result" :text="r.result.replace('_', ' ')" />
              <RouterLink :to="`/stacks/${enc(r.stack_id)}?tab=updates`" class="font-medium hover:underline">{{ r.stack_id }}</RouterLink>
              <span class="muted text-xs">{{ r.services.join(', ') }} · {{ r.trigger }} · {{ ago(r.started_at) }}</span>
              <RouterLink v-if="r.job_id" :to="`/jobs?id=${r.job_id}`" class="ml-auto text-xs text-emerald-700 hover:underline dark:text-emerald-400">output</RouterLink>
            </div>
            <div v-for="c in r.changes" :key="c.service" class="muted mt-0.5 font-mono text-[11px]">{{ c.service }}: {{ shortDigest(c.from) || 'none' }} → {{ shortDigest(c.to) }}</div>
            <div v-if="r.message" class="mt-0.5 text-xs break-words" :class="r.result === 'updated' ? 'muted' : 'text-red-600 dark:text-red-400'">{{ r.message }}</div>
          </li>
        </ul>
      </div>

      <form v-if="settings" class="card self-start" @submit.prevent="saveSettings">
        <div class="card-h">
          Settings
          <button type="button" class="btn btn-sm font-normal" :class="settings.paused ? '!border-amber-500 !text-amber-700 dark:!text-amber-400' : ''" @click="togglePause">
            {{ settings.paused ? 'Resume automatic updates' : 'Pause automatic updates' }}
          </button>
        </div>
        <div class="grid grid-cols-1 gap-4 px-4 py-4 sm:grid-cols-2">
          <div>
            <label class="label" for="check-cron">Check for updates</label>
            <input id="check-cron" v-model="settings.check_cron" class="input font-mono" />
            <div class="mt-1 flex flex-wrap gap-1">
              <button v-for="p in checkPresets" :key="p.v" type="button" class="badge bg-zinc-100 text-zinc-600 hover:bg-zinc-200 dark:bg-zinc-800 dark:text-zinc-400" @click="settings.check_cron = p.v">{{ p.label }}</button>
            </div>
          </div>
          <div>
            <label class="label" for="default-mode">Default policy</label>
            <select id="default-mode" v-model="settings.default_mode" class="input">
              <option value="manual">manual: check only</option>
              <option value="notify">notify: tell me about updates</option>
              <option value="auto">auto: apply in the window</option>
            </select>
            <p class="muted mt-1 text-xs">For stacks without their own policy.</p>
          </div>
          <div>
            <label class="label" for="window-cron">Maintenance window starts</label>
            <input id="window-cron" v-model="settings.window_cron" class="input font-mono" />
            <div class="mt-1 flex flex-wrap gap-1">
              <button v-for="p in windowPresets" :key="p.v" type="button" class="badge bg-zinc-100 text-zinc-600 hover:bg-zinc-200 dark:bg-zinc-800 dark:text-zinc-400" @click="settings.window_cron = p.v">{{ p.label }}</button>
            </div>
          </div>
          <div>
            <label class="label" for="window-min">Window length (minutes)</label>
            <input id="window-min" v-model.number="settings.window_minutes" type="number" min="10" class="input" />
          </div>
          <div>
            <label class="label" for="cooldown">Cooldown (hours)</label>
            <input id="cooldown" v-model.number="settings.cooldown_hours" type="number" min="0" class="input" />
            <p class="muted mt-1 text-xs">New images wait this long before automatic updates, so broken releases get caught first.</p>
          </div>
          <div>
            <label class="label" for="verify">Health check after an update (seconds)</label>
            <input id="verify" v-model.number="settings.verify_seconds" type="number" min="20" class="input" />
          </div>
          <div>
            <label class="label" for="keep">Rollback images kept per service</label>
            <input id="keep" v-model.number="settings.keep_rollbacks" type="number" min="1" max="10" class="input" />
          </div>
          <div>
            <label class="label" for="free">Free disk needed for automatic updates (GiB)</label>
            <input id="free" v-model.number="settings.min_free_gib" type="number" min="0" class="input" />
          </div>
        </div>
        <div class="flex justify-end gap-2 border-t border-zinc-200 px-4 py-2.5 dark:border-zinc-800">
          <span class="muted mr-auto self-center text-xs">Cron times are the server's local time.</span>
          <button class="btn btn-primary btn-sm" :disabled="!dirty || state.info?.read_only">Save settings</button>
        </div>
      </form>
    </div>
  </div>
</template>
