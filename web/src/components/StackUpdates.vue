<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api, type Hook, type Stack, type StackPolicy, type StackUpdates } from '../api'
import { ago, dateTime, shortDigest, shortImage } from '../format'
import { ask, hasUpdate, jobFor, runAction, state, toast } from '../state'
import StatusBadge from './StatusBadge.vue'

const props = defineProps<{ stack: Stack }>()
const data = ref<StackUpdates | null>(null)
const policy = ref<StackPolicy | null>(null)
const saved = ref('')

async function load() {
  try {
    data.value = await api.stackUpdates(props.stack.id)
    policy.value = JSON.parse(JSON.stringify(data.value.policy))
    if (!policy.value!.stack.hooks) policy.value!.stack.hooks = []
    saved.value = JSON.stringify(policy.value)
  } catch (e: any) {
    toast('bad', e.message)
  }
}
load()
watch(() => [state.updates, jobFor(props.stack.id)?.state], () => {
  if (!dirty.value) load()
})

const dirty = computed(() => policy.value && JSON.stringify(policy.value) !== saved.value)
async function savePolicy() {
  if (!policy.value) return
  try {
    data.value = await api.savePolicy(props.stack.id, policy.value)
    policy.value = JSON.parse(JSON.stringify(data.value.policy))
    saved.value = JSON.stringify(policy.value)
    toast('ok', 'Policy saved')
  } catch (e: any) {
    toast('bad', e.message)
  }
}

function addHook(type: Hook['type']) {
  if (type === 'volumes') {
    policy.value!.stack.hooks.push({ type, keep: 3, stop: false, binds: true })
    return
  }
  const db = props.stack.services.find((s) => /postgres|postgis|timescale/.test(s.image))
  policy.value!.stack.hooks.push({ type, service: type === 'pg_dump' ? db?.name ?? props.stack.services[0]?.name ?? '' : props.stack.services[0]?.name ?? '', command: '', keep: 5 })
}
const hookName = (t: string) => (t === 'pg_dump' ? 'pg_dumpall backup' : t === 'volumes' ? 'volume snapshot' : 'command')

async function check() {
  try {
    const j = await api.checkUpdates(props.stack.id)
    state.jobs[j.id] = j
  } catch (e: any) {
    toast('bad', e.message)
  }
}

async function rollback(id: number, label: string) {
  if (!(await ask({ title: `Roll back to ${label}?`, text: 'The service is recreated from the image it ran before. Data volumes are not rolled back.', confirm: 'Roll back', danger: true }))) return
  try {
    const j = await api.rollback(props.stack.id, id)
    state.jobs[j.id] = j
  } catch (e: any) {
    toast('bad', e.message)
  }
}

const stateText: Record<string, string> = {
  up_to_date: 'up to date', update_available: 'update available', pulled_not_applied: 'pulled, not applied', local_build: 'built locally',
  base_updated: 'base image changed', pinned_digest: 'pinned', unknown: 'check failed', not_checked: 'not checked',
}
const runClass: Record<string, string> = { updated: 'ok', unchanged: 'idle', failed: 'failed', rolled_back: 'rolled_back', running: 'running_job' }
const busy = computed(() => !!jobFor(props.stack.id) || !!state.info?.read_only)
const inheritLabel = computed(() => `inherit (${policy.value?.stack.mode || policy.value?.default})`)
</script>

<template>
  <div v-if="!data" class="card muted px-4 py-8 text-center text-sm">Loading…</div>
  <div v-else class="space-y-4">
    <div class="card">
      <div class="card-h">
        Images
        <div class="flex gap-2 font-normal">
          <button class="btn btn-sm" @click="check">Check now</button>
          <button
            class="btn btn-sm btn-primary"
            :disabled="busy || !data.services.some(hasUpdate)"
            @click="runAction(stack, 'update', data!.services.filter(hasUpdate).map((s) => s.service))"
          >Update all</button>
        </div>
      </div>
      <div class="divide-y divide-zinc-100 dark:divide-zinc-800/70">
        <div v-for="u in data.services" :key="u.service" class="flex flex-wrap items-start gap-x-4 gap-y-1 px-4 py-3 text-sm">
          <div class="min-w-[10rem] flex-1">
            <div class="font-medium">{{ u.service }}</div>
            <div class="muted font-mono text-xs break-all">{{ u.build ? 'built: ' + u.image : shortImage(u.image) }}</div>
            <div v-for="b in u.base ?? []" :key="b.image" class="muted font-mono text-xs">FROM {{ shortImage(b.image) }} · {{ stateText[b.state] ?? b.state }}</div>
          </div>
          <div class="min-w-[12rem] flex-1 text-xs">
            <StatusBadge :status="u.state" :text="stateText[u.state] ?? u.state" />
            <div v-if="u.state === 'update_available' && u.local_digest" class="muted mt-1 font-mono">{{ shortDigest(u.local_digest) }} → {{ shortDigest(u.remote_digest) }}<template v-if="u.remote_since"> · new since {{ ago(u.remote_since) }}</template></div>
            <div v-if="u.error" class="mt-1 break-words text-amber-700 dark:text-amber-400">{{ u.error }}</div>
            <div v-if="u.hint_same || u.hint_major" class="muted mt-1">newer tags: <span class="font-mono">{{ [u.hint_same, u.hint_major].filter(Boolean).join(', ') }}</span></div>
            <div v-if="u.auto.blocked" class="muted mt-1">auto: {{ u.auto.blocked }}</div>
            <div v-else-if="u.auto.eligible" class="mt-1 text-emerald-700 dark:text-emerald-400">auto: {{ dateTime(u.auto.at) }}</div>
          </div>
          <button v-if="hasUpdate(u)" class="btn btn-sm btn-primary" :disabled="busy" @click="runAction(stack, 'update', [u.service])">
            {{ u.state === 'pulled_not_applied' ? 'Apply' : u.state === 'base_updated' ? 'Rebuild' : 'Update' }}
          </button>
        </div>
      </div>
    </div>

    <div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <form v-if="policy" class="card self-start" @submit.prevent="savePolicy">
        <div class="card-h">Update policy</div>
        <div class="space-y-4 px-4 py-4 text-sm">
          <div>
            <label class="label" for="mode">This stack</label>
            <select id="mode" v-model="policy.stack.mode" class="input">
              <option value="">default ({{ policy.default }})</option>
              <option value="manual">manual: check only, no messages</option>
              <option value="notify">notify: tell me about updates</option>
              <option value="auto">auto: apply in the maintenance window</option>
            </select>
          </div>
          <div v-if="stack.services.length > 1">
            <div class="label">Per service</div>
            <div class="space-y-1.5">
              <div v-for="sv in stack.services.filter((s) => s.in_file)" :key="sv.name" class="flex items-center gap-2">
                <span class="w-32 truncate font-mono text-xs">{{ sv.name }}</span>
                <select v-model="policy.services[sv.name]" class="input !py-1 text-xs" :aria-label="`Policy of ${sv.name}`">
                  <option :value="undefined">{{ inheritLabel }}</option>
                  <option value="manual">manual</option>
                  <option value="notify">notify</option>
                  <option value="auto">auto</option>
                </select>
              </div>
            </div>
          </div>
          <div class="space-y-2">
            <label class="flex items-start gap-2"><input v-model="policy.stack.auto_rollback" type="checkbox" class="mt-0.5 accent-emerald-600" />
              <span>Roll back automatically when an update doesn't come up healthy</span></label>
            <label class="flex items-start gap-2"><input v-model="policy.stack.include_db" type="checkbox" class="mt-0.5 accent-emerald-600" />
              <span>Include databases in automatic updates <span class="muted">(rolling back an image doesn't roll back its data)</span></span></label>
            <label v-if="stack.services.some((s) => s.build)" class="flex items-start gap-2"><input v-model="policy.stack.rebuild" type="checkbox" class="mt-0.5 accent-emerald-600" />
              <span>Rebuild automatically when a base image changes</span></label>
          </div>
          <div>
            <div class="label">Before every update</div>
            <div v-if="!policy.stack.hooks.length" class="muted text-xs">Nothing runs before updates.</div>
            <div v-for="(h, i) in policy.stack.hooks" :key="i" class="mb-2 space-y-1.5 rounded-lg border border-zinc-200 p-2 dark:border-zinc-800">
              <div class="flex items-center gap-2">
                <span class="badge bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300">{{ hookName(h.type) }}</span>
                <template v-if="h.type !== 'volumes'">
                  <span class="muted text-xs">in</span>
                  <select v-model="h.service" class="input !w-auto !py-1 text-xs" aria-label="Service">
                    <option v-for="sv in stack.services" :key="sv.name" :value="sv.name">{{ sv.name }}</option>
                  </select>
                </template>
                <button type="button" class="ml-auto text-xs text-red-600 hover:underline dark:text-red-400" @click="policy.stack.hooks.splice(i, 1)">remove</button>
              </div>
              <input v-if="h.type === 'command'" v-model="h.command" class="input font-mono !text-xs" placeholder="e.g. php artisan backup:run" aria-label="Command" />
              <div v-else-if="h.type === 'volumes'" class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                <label class="flex items-center gap-1.5"><input v-model="h.binds" type="checkbox" class="accent-emerald-600" />include bind folders</label>
                <label class="flex items-center gap-1.5"><input v-model="h.stop" type="checkbox" class="accent-emerald-600" />stop while copying</label>
                <span class="muted">keep</span>
                <input v-model.number="h.keep" type="number" min="1" max="50" class="input !w-20 !py-1 text-xs" aria-label="Snapshots to keep" />
              </div>
              <div v-else class="flex items-center gap-2 text-xs">
                <span class="muted">keep</span>
                <input v-model.number="h.keep" type="number" min="1" max="50" class="input !w-20 !py-1 text-xs" aria-label="Dumps to keep" />
                <span class="muted">dumps in data_dir/backups</span>
              </div>
            </div>
            <div class="flex gap-2">
              <button type="button" class="btn btn-sm" @click="addHook('pg_dump')">+ pg_dump backup</button>
              <button type="button" class="btn btn-sm" @click="addHook('volumes')">+ volume snapshot</button>
              <button type="button" class="btn btn-sm" @click="addHook('command')">+ command</button>
            </div>
            <p class="muted mt-1 text-xs">A failing step stops the update before anything changes.</p>
          </div>
        </div>
        <div class="flex justify-end border-t border-zinc-200 px-4 py-2.5 dark:border-zinc-800">
          <button class="btn btn-sm btn-primary" :disabled="!dirty || state.info?.read_only">Save policy</button>
        </div>
      </form>

      <div class="space-y-4">
        <div class="card">
          <div class="card-h">Rollback points</div>
          <div v-if="!data.rollbacks.length" class="muted px-4 py-6 text-center text-sm">None yet: each update keeps the image it replaced.</div>
          <ul class="divide-y divide-zinc-100 text-sm dark:divide-zinc-800/70">
            <li v-for="p in data.rollbacks" :key="p.id" class="flex flex-wrap items-center gap-2 px-4 py-2">
              <span class="font-medium">{{ p.service }}</span>
              <span class="muted font-mono text-xs">{{ shortDigest(p.image_id) }}</span>
              <span class="muted text-xs">{{ ago(p.created_at) }}</span>
              <span v-if="p.current" class="badge bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300">running now</span>
              <button v-else class="btn btn-sm ml-auto" :disabled="busy" @click="rollback(p.id, `${p.service} from ${dateTime(p.created_at)}`)">Roll back</button>
            </li>
          </ul>
        </div>
        <div class="card">
          <div class="card-h">History</div>
          <div v-if="!data.runs.length" class="muted px-4 py-6 text-center text-sm">No updates yet.</div>
          <ul class="max-h-[24rem] divide-y divide-zinc-100 overflow-auto text-sm dark:divide-zinc-800/70">
            <li v-for="r in data.runs" :key="r.id" class="px-4 py-2">
              <div class="flex flex-wrap items-center gap-2">
                <StatusBadge :status="runClass[r.result] ?? r.result" :text="r.result.replace('_', ' ')" />
                <span class="text-xs">{{ r.services.join(', ') }}</span>
                <span class="muted text-xs">{{ r.trigger }} · {{ ago(r.started_at) }}</span>
                <button v-if="r.job_id" class="ml-auto text-xs text-emerald-700 hover:underline dark:text-emerald-400" @click="state.jobPanel = r.job_id">output</button>
              </div>
              <div v-for="c in r.changes" :key="c.service" class="muted mt-0.5 font-mono text-[11px]">{{ c.service }}: {{ shortDigest(c.from) || 'none' }} → {{ shortDigest(c.to) }}</div>
              <div v-if="r.message" class="mt-0.5 text-xs break-words" :class="r.result === 'failed' || r.result === 'rolled_back' ? 'text-red-600 dark:text-red-400' : 'muted'">{{ r.message }}</div>
            </li>
          </ul>
        </div>
      </div>
    </div>
  </div>
</template>
