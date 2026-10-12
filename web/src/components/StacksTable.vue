<script setup lang="ts">
import { computed, ref } from 'vue'
import type { Stack, StackAction } from '../api'
import { bytes, enc, pct, shortImage } from '../format'
import { hasUpdate, jobFor, runAction, stackUsage, state, updatesFor } from '../state'
import StatusBadge from './StatusBadge.vue'

const filter = ref('')
const shown = computed(() => {
  const f = filter.value.trim().toLowerCase()
  if (!f) return state.stacks
  return state.stacks.filter(
    (s) => s.id.toLowerCase().includes(f) || s.status.includes(f) || s.services.some((sv) => sv.name.includes(f) || sv.image.toLowerCase().includes(f)),
  )
})

const host = window.location.hostname
function ports(st: Stack) {
  const out: { label: string; href?: string }[] = []
  const seen = new Set<string>()
  for (const sv of st.services)
    for (const c of sv.containers)
      for (const p of c.ports ?? []) {
        if (!p.public) continue
        const key = `${p.public}/${p.type}`
        if (seen.has(key)) continue
        seen.add(key)
        out.push({ label: `${p.public}${p.type === 'udp' ? '/udp' : ''}`, href: p.type === 'tcp' ? `http://${host}:${p.public}` : undefined })
      }
  return out
}

function chips(st: Stack) {
  const out: { text: string; cls: string; title?: string }[] = []
  if (st.self) out.push({ text: 'this app', cls: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300' })
  if (st.external) out.push({ text: 'external', cls: 'bg-sky-100 text-sky-800 dark:bg-sky-950 dark:text-sky-300', title: st.error })
  else if (st.orphaned) out.push({ text: 'orphaned', cls: 'bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300', title: st.error })
  if (st.services.some((s) => s.build)) out.push({ text: 'build', cls: 'bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400', title: 'Built from a Dockerfile' })
  if (st.services.some((s) => s.stateful)) out.push({ text: 'db', cls: 'bg-violet-100 text-violet-700 dark:bg-violet-950 dark:text-violet-300', title: 'Holds data: excluded from auto-update by default' })
  return out
}

interface Btn {
  label: string
  action?: StackAction
  to?: string
  cls?: string
}
function buttons(st: Stack): Btn[] {
  const b: Btn[] = []
  const id = enc(st.id)
  if (st.total > 0) b.push({ label: 'Logs', to: `/stacks/${id}?tab=logs` })
  if (!st.orphaned) b.push({ label: 'Edit', to: `/stacks/${id}?tab=compose` })
  if (st.status === 'invalid') return b
  if (st.status === 'down') {
    if (!st.orphaned) b.push({ label: 'Start', action: 'up', cls: 'btn-primary' })
    return b
  }
  if (st.running === 0) {
    if (!st.external) b.push({ label: 'Start', action: 'start', cls: 'btn-primary' })
    return b
  }
  if (st.external) return b // another project's stack: act on it from its own page
  b.push({ label: 'Restart', action: 'restart' })
  if (updatesFor(st.id).some(hasUpdate)) b.push({ label: 'Update', action: 'update', cls: 'btn-primary' })
  if (!st.self) b.push({ label: 'Stop', action: 'stop', cls: '!text-red-600 dark:!text-red-400' })
  return b
}

const usage = (st: Stack) => stackUsage(st)
const pending = (st: Stack, svc: string) => updatesFor(st.id).some((u) => u.service === svc && hasUpdate(u))
</script>

<template>
  <div class="card">
    <div class="card-h flex-wrap">
      <span>Stacks <span class="font-normal text-zinc-500">({{ state.stacks.length }})</span></span>
      <div class="flex min-w-0 flex-1 items-center justify-end gap-2 font-normal">
        <input v-model="filter" class="input min-w-0 !py-1 text-xs sm:!w-48" placeholder="Filter…" aria-label="Filter stacks" />
        <slot name="actions" />
        <RouterLink v-if="!state.info?.read_only" to="/new" class="btn btn-sm btn-primary">New app</RouterLink>
      </div>
    </div>

    <div v-if="!state.stacksLoaded" class="muted px-4 py-8 text-center text-sm">Loading stacks…</div>
    <div v-else-if="!state.stacks.length" class="muted px-4 py-8 text-center text-sm">
      No compose stacks found in <span class="font-mono">{{ state.info?.apps_root }}</span>.
      <RouterLink to="/new" class="text-emerald-700 hover:underline dark:text-emerald-400">Create the first one</RouterLink>.
    </div>

    <!-- desktop table -->
    <table v-if="shown.length" class="hidden w-full text-sm md:table">
      <thead>
        <tr class="border-b border-zinc-200 text-left text-xs text-zinc-500 dark:border-zinc-800">
          <th class="px-4 py-2 font-medium">Stack</th>
          <th class="px-2 py-2 font-medium">Status</th>
          <th class="px-2 py-2 font-medium">Containers</th>
          <th class="px-2 py-2 font-medium">Images</th>
          <th class="px-2 py-2 font-medium">Ports</th>
          <th class="px-2 py-2 text-right font-medium">CPU / RAM</th>
          <th class="px-4 py-2"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="st in shown"
          :key="st.id"
          class="border-b border-zinc-100 align-middle last:border-0 hover:bg-zinc-50 dark:border-zinc-800/70 dark:hover:bg-zinc-800/30"
        >
          <td class="px-4 py-3">
            <RouterLink :to="`/stacks/${enc(st.id)}`" class="font-medium hover:text-emerald-700 hover:underline dark:hover:text-emerald-400">{{ st.id.startsWith('~') ? st.name : st.id }}</RouterLink>
            <div v-if="chips(st).length" class="mt-1 flex flex-wrap gap-1">
              <span v-for="c in chips(st)" :key="c.text" class="badge !rounded-md !px-1.5 !py-0" :class="c.cls" :title="c.title">{{ c.text }}</span>
            </div>
          </td>
          <td class="px-2 py-3">
            <StatusBadge v-if="jobFor(st.id)" status="running_job" :text="jobFor(st.id)!.kind.replace('_', ' ') + '…'" />
            <StatusBadge v-else :status="st.status" :title="st.error" />
          </td>
          <td class="px-2 py-3 tabular-nums" :class="st.total && st.running < st.total ? 'text-amber-600 dark:text-amber-400' : ''">
            {{ st.total ? `${st.running}/${st.total}` : '–' }}
          </td>
          <td class="max-w-[16rem] px-2 py-3 text-xs text-zinc-600 dark:text-zinc-400">
            <div v-for="sv in st.services.slice(0, 4)" :key="sv.name" class="truncate" :title="`${sv.name}: ${sv.image}`">
              <span v-if="pending(st, sv.name)" class="mr-1 text-sky-600 dark:text-sky-400" title="Update available">⬆</span>{{ sv.build ? `${sv.name} (built)` : shortImage(sv.image) }}
            </div>
            <div v-if="st.services.length > 4" class="text-zinc-400" :title="st.services.slice(4).map((s) => s.image).join('\n')">+ {{ st.services.length - 4 }} more</div>
          </td>
          <td class="px-2 py-3 text-xs">
            <div class="flex max-w-[9rem] flex-wrap gap-x-2">
              <template v-for="p in ports(st)" :key="p.label">
                <a v-if="p.href" :href="p.href" target="_blank" rel="noopener" class="text-sky-700 hover:underline dark:text-sky-400">{{ p.label }}</a>
                <span v-else>{{ p.label }}</span>
              </template>
              <span v-if="!ports(st).length" class="text-zinc-400">–</span>
            </div>
          </td>
          <td class="px-2 py-3 text-right text-xs whitespace-nowrap text-zinc-600 tabular-nums dark:text-zinc-400">
            <template v-if="st.running">{{ pct(usage(st).cpu) }} · {{ bytes(usage(st).mem, 0) }}</template>
            <template v-else>–</template>
          </td>
          <td class="px-4 py-3">
            <div class="flex flex-wrap justify-end gap-1.5">
              <template v-for="b in buttons(st)" :key="b.label">
                <RouterLink v-if="b.to" :to="b.to" class="btn btn-sm">{{ b.label }}</RouterLink>
                <button v-else class="btn btn-sm" :class="b.cls" :disabled="!!jobFor(st.id) || state.info?.read_only" @click="runAction(st, b.action!)">{{ b.label }}</button>
              </template>
            </div>
          </td>
        </tr>
      </tbody>
    </table>

    <!-- phones: stacked list -->
    <ul v-if="shown.length" class="divide-y divide-zinc-100 md:hidden dark:divide-zinc-800">
      <li v-for="st in shown" :key="st.id" class="space-y-2 px-4 py-3">
        <div class="flex items-start justify-between gap-2">
          <div class="min-w-0">
            <RouterLink :to="`/stacks/${enc(st.id)}`" class="font-medium">{{ st.id.startsWith('~') ? st.name : st.id }}</RouterLink>
            <div class="muted text-xs">
              {{ st.total ? `${st.running}/${st.total} running` : 'no containers' }}
              <template v-if="st.running"> · {{ pct(usage(st).cpu) }} · {{ bytes(usage(st).mem, 0) }}</template>
            </div>
          </div>
          <StatusBadge v-if="jobFor(st.id)" status="running_job" :text="jobFor(st.id)!.kind + '…'" />
          <StatusBadge v-else :status="st.status" />
        </div>
        <div v-if="chips(st).length" class="flex flex-wrap gap-1">
          <span v-for="c in chips(st)" :key="c.text" class="badge !rounded-md !px-1.5 !py-0" :class="c.cls">{{ c.text }}</span>
        </div>
        <div class="flex flex-wrap gap-1.5">
          <template v-for="b in buttons(st)" :key="b.label">
            <RouterLink v-if="b.to" :to="b.to" class="btn btn-sm">{{ b.label }}</RouterLink>
            <button v-else class="btn btn-sm" :class="b.cls" :disabled="!!jobFor(st.id) || state.info?.read_only" @click="runAction(st, b.action!)">{{ b.label }}</button>
          </template>
        </div>
      </li>
    </ul>
    <div v-if="state.stacks.length && !shown.length" class="muted px-4 py-6 text-center text-sm">Nothing matches “{{ filter }}”.</div>
  </div>
</template>
