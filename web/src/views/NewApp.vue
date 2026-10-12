<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, type ValidationError } from '../api'
import { createEditor } from '../editor/cm'
import { enc } from '../format'
import { ask, state, toast } from '../state'

// A new app: a folder under apps_root with a compose file (and maybe a .env), optionally started.
const router = useRouter()
const root = ref('')
const depth = ref(3)
const groups = ref<string[]>([])
const group = ref('')
const name = ref('')
const start = ref(true)
const withEnv = ref(false)
const compose = ref('')
const env = ref('')
const check = ref<'idle' | 'checking' | 'ok' | 'error'>('idle')
const vError = ref<ValidationError | null>(null)
const creating = ref(false)
const edited = ref(false)

const path = computed(() => (group.value ? `${group.value}/${name.value.trim()}` : name.value.trim()))
const service = computed(() => (name.value.trim().toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^-+|-+$/g, '') || 'app'))

// ports other apps publish already, to suggest a free one
const usedPorts = computed(() => {
  const s = new Set<number>()
  for (const st of state.stacks) for (const sv of st.services) for (const c of sv.containers) for (const p of c.ports ?? []) if (p.public) s.add(p.public)
  for (const c of state.standalone) for (const p of c.ports ?? []) if (p.public) s.add(p.public)
  return [...s].sort((a, b) => a - b)
})
const freePort = computed(() => {
  let p = 8100
  while (usedPorts.value.includes(p)) p++
  return p
})

const quick = reactive({ image: '', containerPort: '', hostPort: '', volume: true, restart: 'unless-stopped' })

function generate(): string {
  const lines = ['services:', `  ${service.value}:`, `    image: ${quick.image.trim() || 'nginx:alpine'}`, `    restart: ${quick.restart}`]
  if (quick.containerPort.trim()) lines.push('    ports:', `      - "${quick.hostPort.trim() || freePort.value}:${quick.containerPort.trim()}"`)
  if (quick.volume) lines.push('    volumes:', '      - ./data:/data')
  if (withEnv.value) lines.push('    env_file: .env')
  return lines.join('\n') + '\n'
}

const showGen = ref(false) // the "generate from an image" form, for those who don't paste
const generated = ref(false) // the editor holds a generated file (follows name and .env changes until edited)

async function fill() {
  if (edited.value && compose.value.trim() && !(await ask({ title: 'Replace the compose file?', text: 'Your edits in the editor are replaced by the generated file.', confirm: 'Replace', danger: true }))) return
  setCompose(generate())
  edited.value = false
  generated.value = true
  showGen.value = false
}

const composeEl = ref<HTMLElement | null>(null)
const envEl = ref<HTMLElement | null>(null)
let composeEd: ReturnType<typeof createEditor> | null = null
let envEd: ReturnType<typeof createEditor> | null = null
let settingDoc = false

function setCompose(doc: string) {
  settingDoc = true
  compose.value = doc
  composeEd?.setDoc(doc)
  settingDoc = false
  scheduleValidate()
}

onMounted(async () => {
  try {
    const f = await api.appFolders()
    root.value = f.root
    depth.value = f.depth
    groups.value = f.folders
  } catch (e: any) {
    toast('bad', e.message)
  }
  await nextTick()
  composeEd = createEditor(composeEl.value!, {
    doc: '',
    lang: 'yaml',
    placeholder: 'Paste your compose file here…',
    onChange: (d) => {
      compose.value = d
      if (!settingDoc) edited.value = true
      scheduleValidate()
    },
    onSave: () => create(),
  })
})
onBeforeUnmount(() => {
  composeEd?.destroy()
  envEd?.destroy()
})

watch(withEnv, async (on) => {
  if (!on) {
    envEd?.destroy()
    envEd = null
    env.value = ''
    scheduleValidate()
    return
  }
  await nextTick()
  envEd = createEditor(envEl.value!, { doc: env.value, lang: 'env', placeholder: 'KEY=value', onChange: (d) => ((env.value = d), scheduleValidate()) })
  if (generated.value && !edited.value) setCompose(generate()) // adds env_file: .env
})
watch(name, () => {
  if (generated.value && !edited.value) setCompose(generate()) // the service is named after the folder
  else scheduleValidate()
})
watch(group, () => scheduleValidate())

let timer: number | undefined
let seq = 0
function scheduleValidate() {
  check.value = 'idle'
  window.clearTimeout(timer)
  timer = window.setTimeout(validate, 700)
}
async function validate() {
  if (!name.value.trim() || !compose.value.trim()) {
    check.value = 'idle'
    return
  }
  const n = ++seq
  check.value = 'checking'
  try {
    const r = await api.validateApp({ path: path.value, compose: compose.value, env: withEnv.value ? env.value : undefined })
    if (n !== seq) return
    check.value = r.ok ? 'ok' : 'error'
    vError.value = r.error ?? null
    composeEd?.mark(r.error?.line, r.error?.column, r.error?.message)
  } catch (e: any) {
    if (n !== seq) return
    check.value = 'error'
    vError.value = { message: e.message }
  }
}

async function create() {
  if (!name.value.trim() || !compose.value.trim() || creating.value) return
  creating.value = true
  try {
    const r = await api.createApp({ path: path.value, compose: compose.value, env: withEnv.value && env.value ? env.value : undefined, start: start.value })
    if (r.job) state.jobs[r.job.id] = r.job
    toast('ok', `${r.stack} created${r.job ? ': starting…' : ''}`)
    router.push(`/stacks/${enc(r.stack)}`)
  } catch (e: any) {
    check.value = 'error'
    vError.value = { message: e.message, line: e.body?.line }
    composeEd?.mark(e.body?.line, e.body?.column, e.message)
  } finally {
    creating.value = false
  }
}
</script>

<template>
  <div class="space-y-4">
    <div class="card">
      <div class="card-h">New app</div>
      <div class="space-y-3 px-4 py-4 text-sm">
        <div>
          <label class="label" for="app-name">Folder</label>
          <div class="flex max-w-2xl gap-2">
            <select v-if="groups.length" v-model="group" class="input !w-auto max-w-[45%] shrink-0 font-mono" aria-label="Inside">
              <option value="">top level</option>
              <option v-for="g in groups" :key="g" :value="g">{{ g }}/</option>
            </select>
            <input id="app-name" v-model="name" class="input min-w-0 flex-1 font-mono" placeholder="myapp" autocomplete="off" spellcheck="false" autofocus />
          </div>
          <p class="muted mt-1 text-xs">
            The app lives in <span class="font-mono">{{ root }}/{{ path || '…' }}</span>. An existing folder without a compose file is taken over as it is (handy when you copied files there first).
          </p>
        </div>
        <div class="flex flex-wrap gap-x-6 gap-y-2">
          <label class="flex items-center gap-2"><input v-model="withEnv" type="checkbox" class="accent-emerald-600" />Add a <span class="font-mono">.env</span> file (secrets and settings)</label>
          <label class="flex items-center gap-2"><input v-model="start" type="checkbox" class="accent-emerald-600" />Start it right away (pulls the images)</label>
        </div>
      </div>
    </div>

    <div class="card flex flex-col">
      <div class="card-h">
        <span class="font-mono">compose.yaml</span>
        <button class="btn btn-sm font-normal" :aria-expanded="showGen" @click="showGen = !showGen">{{ showGen ? 'Hide' : 'Generate from an image…' }}</button>
      </div>
      <div v-if="showGen" class="border-b border-zinc-200 bg-zinc-50/60 px-4 py-3 text-sm dark:border-zinc-800 dark:bg-zinc-950/40">
        <div class="grid grid-cols-1 gap-2 sm:grid-cols-[2fr_1fr_1fr]">
          <div>
            <label class="label" for="q-image">Image</label>
            <input id="q-image" v-model="quick.image" class="input font-mono" placeholder="ghcr.io/owner/app:latest" spellcheck="false" />
          </div>
          <div>
            <label class="label" for="q-cport">Port in the container</label>
            <input id="q-cport" v-model="quick.containerPort" class="input font-mono" placeholder="80" inputmode="numeric" />
          </div>
          <div>
            <label class="label" for="q-hport">Port on the server</label>
            <input id="q-hport" v-model="quick.hostPort" class="input font-mono" :placeholder="String(freePort)" inputmode="numeric" />
          </div>
        </div>
        <div class="mt-2 flex flex-wrap items-center gap-x-4 gap-y-2">
          <label class="flex items-center gap-2 text-xs"><input v-model="quick.volume" type="checkbox" class="accent-emerald-600" />keep data in <span class="font-mono">./data</span></label>
          <select v-model="quick.restart" class="input !w-auto !py-1 text-xs" aria-label="Restart policy">
            <option value="unless-stopped">restart: unless-stopped</option>
            <option value="always">restart: always</option>
            <option value="on-failure">restart: on-failure</option>
            <option value="no">restart: no</option>
          </select>
          <span v-if="usedPorts.length" class="muted text-[11px]">Ports in use: {{ usedPorts.join(', ') }}</span>
          <button class="btn btn-sm btn-primary ml-auto" @click="fill">Fill in compose file</button>
        </div>
      </div>
      <div ref="composeEl" class="min-h-[18rem] overflow-hidden" style="height: 40vh" />
      <div class="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-b-xl border-t border-zinc-200 px-4 py-2 text-xs dark:border-zinc-800">
        <span v-if="!compose.trim()" class="muted">Paste or write the compose file (or generate one from an image).</span>
        <span v-else-if="!name.trim()" class="muted">Name the folder to check the file.</span>
        <span v-else-if="check === 'checking'" class="muted">Checking with docker compose…</span>
        <span v-else-if="check === 'ok'" class="text-emerald-700 dark:text-emerald-400">✓ valid</span>
        <button v-else-if="check === 'error' && vError" class="min-w-0 text-left text-red-600 hover:underline dark:text-red-400" @click="vError?.line && composeEd?.goto(vError.line)">
          ✕ <template v-if="vError.line">line {{ vError.line }}: </template>{{ vError.message }}
        </button>
        <span class="muted ml-auto">Ctrl+S creates the app</span>
      </div>
    </div>

    <div v-if="withEnv" class="card flex flex-col">
      <div class="card-h"><span class="font-mono">.env</span></div>
      <div ref="envEl" class="min-h-[8rem] overflow-hidden" style="height: 16vh" />
    </div>

    <div class="flex flex-wrap items-center justify-end gap-2">
      <RouterLink to="/" class="btn">Cancel</RouterLink>
      <button class="btn btn-primary" :disabled="!name.trim() || !compose.trim() || creating || check === 'error' || state.info?.read_only" @click="create">
        {{ creating ? 'Creating…' : start ? 'Create and start' : 'Create' }}
      </button>
    </div>
  </div>
</template>
