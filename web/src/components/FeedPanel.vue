<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { api, type FeedItem } from '../api'
import { enc, time } from '../format'
import { state, toast } from '../state'

// The Events card: Docker events and job messages as they happen.
const props = withDefaults(defineProps<{ stack?: string; height?: string; title?: string }>(), { height: '20rem', title: 'Events' })
const filter = ref('')
const paused = ref(false)
const frozen = ref<FeedItem[] | null>(null)
const box = ref<HTMLElement | null>(null)

watch(paused, (p) => (frozen.value = p ? [...state.feed] : null))

const shown = computed(() => {
  let items = frozen.value ?? state.feed
  if (props.stack) items = items.filter((i) => i.stack === props.stack)
  const f = filter.value.toLowerCase()
  if (f) items = items.filter((i) => `${i.stack} ${i.container} ${i.action} ${i.message}`.toLowerCase().includes(f))
  return items.slice(-400)
})

async function reload() {
  try {
    state.feed = (await api.feed()).items
  } catch (e: any) {
    toast('bad', e.message)
  }
}

watch(
  () => shown.value.length,
  async () => {
    const el = box.value
    if (!el) return
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40
    await nextTick()
    if (atBottom) el.scrollTop = el.scrollHeight
  },
  { flush: 'pre' },
)

const levelClass = (l: string) => (l === 'error' ? 'text-red-400' : l === 'warn' ? 'text-amber-300' : '')
</script>

<template>
  <div class="card flex flex-col">
    <div class="card-h flex-wrap">
      <span class="flex items-center gap-2">
        {{ title }}
        <span class="h-1.5 w-1.5 rounded-full" :class="state.eventsConnected && !paused ? 'bg-emerald-500' : 'bg-zinc-400'" />
      </span>
      <div class="flex min-w-0 flex-1 items-center justify-end gap-2 font-normal">
        <input v-model="filter" class="input min-w-0 !py-1 text-xs sm:!w-44" placeholder="Filter…" aria-label="Filter events" />
        <button class="btn btn-sm" @click="paused = !paused">{{ paused ? 'Resume' : 'Pause' }}</button>
        <button class="btn btn-sm" @click="reload">Reload</button>
      </div>
    </div>
    <div ref="box" class="overflow-auto rounded-b-xl bg-zinc-950 px-3 py-2 font-mono text-[11.5px] leading-[1.55] text-zinc-300" :style="{ height }">
      <div v-for="(it, i) in shown" :key="i" class="whitespace-pre-wrap break-all">
        <span class="text-zinc-500">{{ time(it.time) }}</span>
        <RouterLink v-if="it.stack && !stack" :to="`/stacks/${enc(it.stack)}`" class="ml-1.5 text-sky-400 hover:underline">{{ it.stack }}</RouterLink>
        <span v-if="it.container" class="ml-1.5 text-violet-300">{{ it.container }}</span>
        <span class="ml-1.5" :class="levelClass(it.level)">{{ it.message || it.action }}</span>
      </div>
      <div v-if="!shown.length" class="text-zinc-500">No events yet. Start, stop or update something and it shows up here.</div>
    </div>
  </div>
</template>
