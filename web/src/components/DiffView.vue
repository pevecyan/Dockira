<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { EditorView } from '@codemirror/view'
import { createDiff, type Lang } from '../editor/cm'

// Read-only unified diff (old lines struck through inline, unchanged runs collapsed).
const props = withDefaults(defineProps<{ original: string; modified: string; lang?: Lang; height?: string }>(), { lang: 'yaml', height: '24rem' })
const el = ref<HTMLElement | null>(null)
let view: EditorView | null = null

function render() {
  view?.destroy()
  if (el.value) view = createDiff(el.value, props.original, props.modified, props.lang)
}
onMounted(render)
watch(() => [props.original, props.modified, props.lang], render)
onBeforeUnmount(() => view?.destroy())
</script>

<template>
  <div>
    <div v-if="original === modified" class="muted px-4 py-6 text-center text-sm">No differences.</div>
    <div v-show="original !== modified" ref="el" class="overflow-auto" :style="{ height }" />
  </div>
</template>
