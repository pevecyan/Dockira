<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import Icon from './Icon.vue'

const props = withDefaults(defineProps<{ title: string; wide?: boolean; full?: boolean; closable?: boolean }>(), { closable: true })
const emit = defineEmits<{ close: [] }>()
const box = ref<HTMLElement | null>(null)

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape' && props.closable) emit('close')
}
onMounted(() => {
  document.addEventListener('keydown', onKey)
  // focus the first field (or the dialog) so Enter/Escape work right away
  const first = box.value?.querySelector<HTMLElement>('input, textarea, select, [autofocus]')
  ;(first ?? box.value)?.focus()
})
onBeforeUnmount(() => document.removeEventListener('keydown', onKey))
</script>

<template>
  <Teleport to="body">
    <div class="fixed inset-0 z-[60] flex items-end justify-center bg-black/50 p-0 sm:items-center sm:p-4" @mousedown.self="closable && emit('close')">
      <div
        ref="box"
        tabindex="-1"
        role="dialog"
        :aria-label="title"
        class="card flex max-h-[92vh] w-full flex-col overflow-hidden shadow-2xl outline-none sm:max-h-[85vh]"
        :class="full ? 'sm:max-w-6xl' : wide ? 'sm:max-w-3xl' : 'sm:max-w-md'"
      >
        <div class="card-h">
          <span class="truncate">{{ title }}</span>
          <div class="flex items-center gap-2 font-normal">
            <slot name="actions" />
            <button v-if="closable" class="rounded-lg p-1 text-zinc-500 hover:bg-zinc-100 dark:hover:bg-zinc-800" aria-label="Close" @click="emit('close')">
              <Icon name="close" :size="16" />
            </button>
          </div>
        </div>
        <div class="min-h-0 flex-1 overflow-auto">
          <slot />
        </div>
        <div v-if="$slots.footer" class="flex flex-wrap items-center justify-end gap-2 border-t border-zinc-200 px-4 py-2.5 dark:border-zinc-800">
          <slot name="footer" />
        </div>
      </div>
    </div>
  </Teleport>
</template>
