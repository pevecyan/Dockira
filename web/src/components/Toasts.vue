<script setup lang="ts">
import { dismiss, state } from '../state'
import Icon from './Icon.vue'

const tones = {
  ok: 'border-emerald-300 dark:border-emerald-900',
  info: 'border-sky-300 dark:border-sky-900',
  warn: 'border-amber-300 dark:border-amber-900',
  bad: 'border-red-300 dark:border-red-900',
}
const dots = { ok: 'bg-emerald-500', info: 'bg-sky-500 animate-pulse', warn: 'bg-amber-500', bad: 'bg-red-500' }

function open(id: number, jobId?: number) {
  if (jobId) state.jobPanel = jobId
  else dismiss(id)
}
</script>

<template>
  <div class="pointer-events-none fixed inset-x-0 bottom-0 z-[55] flex flex-col items-center gap-2 p-3 sm:inset-x-auto sm:right-0 sm:items-end">
    <TransitionGroup
      enter-from-class="translate-y-2 opacity-0"
      enter-active-class="transition duration-200"
      leave-to-class="opacity-0"
      leave-active-class="transition duration-150"
    >
      <div
        v-for="t in state.toasts"
        :key="t.id"
        class="pointer-events-auto flex w-full max-w-sm items-start gap-2.5 rounded-xl border bg-white px-3 py-2.5 text-sm shadow-lg dark:bg-zinc-900"
        :class="[tones[t.tone], t.jobId ? 'cursor-pointer hover:bg-zinc-50 dark:hover:bg-zinc-800' : '']"
        role="status"
        @click="open(t.id, t.jobId)"
      >
        <span class="mt-1.5 h-2 w-2 shrink-0 rounded-full" :class="dots[t.tone]" />
        <div class="min-w-0 flex-1">
          <div class="break-words">{{ t.text }}</div>
          <div v-if="t.jobId" class="muted text-xs">Show output</div>
        </div>
        <button class="-mr-1 rounded p-0.5 text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-200" aria-label="Dismiss" @click.stop="dismiss(t.id)">
          <Icon name="close" :size="14" />
        </button>
      </div>
    </TransitionGroup>
  </div>
</template>
