<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { api } from '../api'
import { state } from '../state'
import Modal from './Modal.vue'

// A shell in a container (xterm.js over the exec WebSocket). Asks for the password first.
const props = defineProps<{ container: string; name: string }>()
const emit = defineEmits<{ close: [] }>()
const el = ref<HTMLElement | null>(null)
const status = ref('connecting…')
let term: Terminal | null = null
let ws: WebSocket | null = null
let fit: FitAddon | null = null
let ro: ResizeObserver | null = null

async function confirmed(): Promise<boolean> {
  const s = await api.session()
  if (s.confirmed) return true
  return new Promise((resolve) => (state.passwordPrompt = resolve))
}

async function connect() {
  if (!(await confirmed())) {
    emit('close')
    return
  }
  term = new Terminal({
    cursorBlink: true,
    fontFamily: 'ui-monospace, "JetBrains Mono", "SF Mono", Menlo, Consolas, monospace',
    fontSize: 13,
    theme: { background: '#09090b', foreground: '#e4e4e7', cursor: '#34d399', selectionBackground: '#065f4680' },
  })
  fit = new FitAddon()
  term.loadAddon(fit)
  term.open(el.value!)
  fit.fit()
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  ws = new WebSocket(`${proto}://${location.host}/api/exec/${encodeURIComponent(props.container)}?cols=${term.cols}&rows=${term.rows}`)
  ws.binaryType = 'arraybuffer'
  ws.onopen = () => {
    status.value = 'connected'
    term?.focus()
  }
  ws.onmessage = (e) => term?.write(typeof e.data === 'string' ? e.data : new Uint8Array(e.data))
  ws.onclose = (e) => {
    status.value = e.reason || (e.code === 1000 ? 'the shell exited' : 'disconnected')
    term?.write(`\r\n\x1b[90m[${status.value}]\x1b[0m\r\n`)
  }
  const enc = new TextEncoder()
  term.onData((d) => ws?.readyState === WebSocket.OPEN && ws.send(enc.encode(d)))
  term.onResize(({ cols, rows }) => ws?.readyState === WebSocket.OPEN && ws.send(JSON.stringify({ type: 'resize', cols, rows })))
  ro = new ResizeObserver(() => fit?.fit())
  ro.observe(el.value!)
}

onMounted(connect)
onBeforeUnmount(() => {
  ro?.disconnect()
  ws?.close()
  term?.dispose()
})
</script>

<template>
  <Modal :title="`Shell: ${name}`" full @close="emit('close')">
    <template #actions><span class="text-xs text-zinc-500">{{ status }}</span></template>
    <div class="bg-zinc-950 p-2" style="height: 65vh">
      <div ref="el" class="h-full w-full" />
    </div>
  </Modal>
</template>
