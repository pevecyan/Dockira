<script setup lang="ts">
import { computed, ref } from 'vue'
import { api } from '../../api'
import { ago } from '../../format'
import { state, toast } from '../../state'

const u = computed(() => state.updates)
const busy = ref(false)
async function check() {
  busy.value = true
  try {
    const j = await api.checkUpdates()
    state.jobs[j.id] = j
  } catch (e: any) {
    toast('bad', e.message)
  } finally {
    busy.value = false
  }
}
const at = (t?: string) => (t ? new Date(t).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit', hour12: false }) : '')
const headline = computed(() => {
  if (!u.value || u.value.checked === 0) return 'Not checked yet'
  if (u.value.available) return `${u.value.available} available`
  return 'All up to date'
})
const dot = computed(() => (!u.value?.checked ? 'bg-zinc-400' : u.value.available ? 'bg-sky-500' : u.value.errors ? 'bg-amber-500' : 'bg-emerald-500'))
</script>

<template>
  <div class="card flex flex-col p-4">
    <div class="flex items-start justify-between gap-2">
      <div class="min-w-0">
        <div class="text-xs font-medium uppercase tracking-wide text-zinc-500">Updates</div>
        <div class="mt-0.5 flex items-center gap-2 text-lg font-semibold">
          <span class="h-2.5 w-2.5 shrink-0 rounded-full" :class="[dot, u?.checking ? 'animate-pulse' : '']" />
          <span class="truncate">{{ u?.checking ? 'Checking…' : headline }}</span>
        </div>
      </div>
      <div class="shrink-0 text-right text-xs text-zinc-500 tabular-nums">
        <div v-if="u?.last_check">checked {{ ago(u.last_check) }}</div>
        <div v-if="u?.next_check">next {{ at(u.next_check) }}</div>
      </div>
    </div>
    <div class="muted mt-1 text-sm">
      <template v-if="u?.errors">{{ u.errors }} couldn't be checked · </template>{{ u?.checked ?? 0 }}/{{ u?.total ?? 0 }} images checked
    </div>
    <div class="mt-auto border-t border-zinc-200 pt-3 dark:border-zinc-800" :class="'mt-4'">
      <div class="flex items-end justify-between gap-2">
        <div class="min-w-0 text-xs">
          <div class="text-zinc-500">automatic updates</div>
          <div v-if="u?.paused" class="font-medium text-amber-600 dark:text-amber-400">paused</div>
          <div v-else class="truncate font-medium">
            window {{ at(u?.window) || '–' }}<template v-if="u?.auto_next"> · {{ u.auto_next }} queued</template>
          </div>
        </div>
        <div class="flex shrink-0 gap-1.5">
          <RouterLink to="/updates" class="btn btn-sm">Review</RouterLink>
          <button class="btn btn-sm" :disabled="busy || u?.checking" @click="check">Check now</button>
        </div>
      </div>
    </div>
  </div>
</template>
