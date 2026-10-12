<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'
import Icon from './Icon.vue'

// "⋯" dropdown for less common actions.
export interface Item {
  label: string
  danger?: boolean
  disabled?: boolean
  title?: string
  run: () => void
}
defineProps<{ items: Item[]; label?: string }>()
const open = ref(false)
const root = ref<HTMLElement | null>(null)
function onDoc(e: MouseEvent) {
  if (root.value && !root.value.contains(e.target as Node)) open.value = false
}
document.addEventListener('mousedown', onDoc)
onBeforeUnmount(() => document.removeEventListener('mousedown', onDoc))
</script>

<template>
  <div ref="root" class="relative">
    <button class="btn btn-sm" :aria-expanded="open" aria-haspopup="menu" :title="label ?? 'More actions'" @click="open = !open">
      <template v-if="label">{{ label }}</template><Icon v-else name="more" :size="16" />
    </button>
    <div v-if="open" role="menu" class="absolute right-0 z-40 mt-1 min-w-44 overflow-hidden rounded-lg border border-zinc-200 bg-white py-1 text-sm shadow-lg dark:border-zinc-700 dark:bg-zinc-900">
      <button
        v-for="it in items"
        :key="it.label"
        role="menuitem"
        class="block w-full px-3 py-1.5 text-left hover:bg-zinc-100 disabled:cursor-not-allowed disabled:opacity-50 dark:hover:bg-zinc-800"
        :class="it.danger ? 'text-red-600 dark:text-red-400' : ''"
        :disabled="it.disabled"
        :title="it.title"
        @click="open = false; it.run()"
      >
        {{ it.label }}
      </button>
    </div>
  </div>
</template>
