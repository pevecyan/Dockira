<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'
import { api, ApiError, type EditableFile, type Stack, type ValidationError } from '../api'
import { createEditor } from '../editor/cm'
import { ago } from '../format'
import { ask, state, toast } from '../state'
import DiffView from './DiffView.vue'
import Modal from './Modal.vue'

// The stack's compose file(s); .env and everything else is in the Files tab.
const props = defineProps<{ stack: Stack }>()

const files = ref<EditableFile[]>([])
const name = ref('')
const original = ref('')
const draft = ref('')
const sha = ref('')
const loaded = ref(false)
const loadError = ref('')

type Check = 'idle' | 'checking' | 'ok' | 'error'
const check = ref<Check>('idle')
const vError = ref<ValidationError | null>(null)
const saving = ref(false)
const preview = ref(false)
const conflict = ref<{ theirs: string; sha: string } | null>(null)
const showTheirs = ref(false)

const dirty = computed(() => loaded.value && draft.value !== original.value)
const choices = computed(() => files.value.filter((f) => f.kind === 'compose' || f.kind === 'override'))
const readOnly = computed(() => !!state.info?.read_only)

const el = ref<HTMLElement | null>(null)
let ed: ReturnType<typeof createEditor> | null = null

async function loadFiles() {
  try {
    files.value = (await api.files(props.stack.id)).files
    if (!name.value || !choices.value.some((f) => f.name === name.value)) name.value = choices.value[0]?.name ?? ''
  } catch (e: any) {
    loadError.value = e.message
  }
}

async function load() {
  loadError.value = ''
  loaded.value = false
  conflict.value = null
  try {
    const f = await api.file(props.stack.id, name.value)
    original.value = f.content
    draft.value = f.content
    sha.value = f.sha256
    loaded.value = true
    await nextTick()
    mount()
    validate()
  } catch (e: any) {
    loadError.value = e instanceof ApiError && e.status === 403 ? 'Password confirmation cancelled.' : e.message
  }
}

function mount() {
  if (!el.value) return
  ed?.destroy()
  ed = createEditor(el.value, {
    doc: draft.value,
    lang: 'yaml',
    readOnly: readOnly.value,
    placeholder: 'services:\n  app:\n    image: …',
    onChange: (d) => {
      draft.value = d
      scheduleValidate()
    },
    onSave: () => openPreview(),
  })
}

let vTimer: number | undefined
let vSeq = 0
function scheduleValidate() {
  check.value = 'idle'
  window.clearTimeout(vTimer)
  vTimer = window.setTimeout(validate, 1000)
}

async function validate() {
  const seq = ++vSeq
  check.value = 'checking'
  try {
    const r = await api.validate(props.stack.id, name.value, draft.value)
    if (seq !== vSeq) return
    check.value = r.ok ? 'ok' : 'error'
    vError.value = r.error ?? null
    ed?.mark(r.error?.line, r.error?.column, r.error?.message)
  } catch (e: any) {
    if (seq !== vSeq) return
    check.value = 'error'
    vError.value = { message: e.message }
  }
}

function openPreview() {
  if (!dirty.value || readOnly.value) return
  preview.value = true
}

async function save(apply: boolean, base = sha.value) {
  saving.value = true
  try {
    const r = await api.save(props.stack.id, name.value, draft.value, base, apply)
    original.value = draft.value
    sha.value = r.sha256
    preview.value = false
    conflict.value = null
    toast('ok', `${name.value} saved${apply ? ': applying…' : ''}`)
    if (r.job) state.jobs[r.job.id] = r.job
    loadFiles()
  } catch (e: any) {
    if (e instanceof ApiError && e.status === 409) {
      preview.value = false
      try {
        const cur = await api.file(props.stack.id, name.value)
        conflict.value = { theirs: cur.content, sha: cur.sha256 }
      } catch {
        conflict.value = { theirs: '', sha: e.body?.current_sha256 ?? '' }
      }
    } else if (e instanceof ApiError && e.status === 422) {
      preview.value = false
      check.value = 'error'
      vError.value = { message: e.message, line: e.body?.line, column: e.body?.column }
      ed?.mark(e.body?.line, e.body?.column, e.message)
      toast('bad', 'Not saved: the file is invalid')
    } else {
      toast('bad', e.message)
    }
  } finally {
    saving.value = false
  }
}

async function takeTheirs() {
  if (!conflict.value) return
  original.value = draft.value = conflict.value.theirs
  sha.value = conflict.value.sha
  ed?.setDoc(conflict.value.theirs)
  conflict.value = null
  validate()
}

async function overwrite() {
  if (!conflict.value) return
  if (await ask({ title: 'Overwrite their changes?', text: 'The version saved elsewhere is replaced by yours. It stays in History.', confirm: 'Overwrite', danger: true }))
    save(false, conflict.value.sha)
}

async function revert() {
  if (await ask({ title: 'Discard your changes?', text: 'The editor goes back to the saved file.', confirm: 'Discard', danger: true })) {
    draft.value = original.value
    ed?.setDoc(original.value)
    validate()
  }
}

async function switchFile(n: string) {
  if (n === name.value) return
  if (dirty.value && !(await ask({ title: 'Discard unsaved changes?', text: `Your changes to ${name.value} are lost.`, confirm: 'Discard', danger: true }))) return
  name.value = n
  load()
}

onMounted(async () => {
  await loadFiles()
  if (name.value) load()
})
onBeforeUnmount(() => ed?.destroy())
watch(readOnly, (r) => ed?.setReadOnly(r))

onBeforeRouteLeave(async () => (dirty.value ? await ask({ title: 'Leave without saving?', text: 'Your changes are lost.', confirm: 'Leave', danger: true }) : true))
function beforeUnload(e: BeforeUnloadEvent) {
  if (dirty.value) e.preventDefault()
}
window.addEventListener('beforeunload', beforeUnload)
onBeforeUnmount(() => window.removeEventListener('beforeunload', beforeUnload))

const current = computed(() => files.value.find((f) => f.name === name.value))
</script>

<template>
  <div class="card flex flex-col">
    <div class="card-h flex-wrap">
      <div class="flex min-w-0 items-center gap-1">
        <template v-if="choices.length > 1">
          <button
            v-for="f in choices"
            :key="f.name"
            class="rounded-md px-2 py-0.5 font-mono text-xs font-normal"
            :class="f.name === name ? 'bg-zinc-200 text-zinc-900 dark:bg-zinc-700 dark:text-zinc-100' : 'text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200'"
            @click="switchFile(f.name)"
          >
            {{ f.name }}
          </button>
        </template>
        <span v-else class="font-mono text-sm">{{ name || 'compose file' }}</span>
        <span v-if="dirty" class="ml-1 h-2 w-2 rounded-full bg-amber-500" title="Unsaved changes" />
      </div>
      <div v-if="loaded" class="flex flex-wrap items-center gap-1.5 font-normal">
        <button class="btn btn-sm" :disabled="check === 'checking'" @click="validate">Validate</button>
        <button class="btn btn-sm" :disabled="!dirty" @click="revert">Revert</button>
        <button class="btn btn-sm btn-primary" :disabled="!dirty || saving || readOnly" title="Save (Ctrl+S)" @click="openPreview">Save…</button>
      </div>
    </div>

    <div v-if="loadError" class="px-4 py-3 text-sm text-red-600 dark:text-red-400">{{ loadError }}</div>

    <!-- conflict -->
    <div v-if="conflict" class="border-b border-amber-300 bg-amber-50 px-4 py-3 text-sm dark:border-amber-900 dark:bg-amber-950/40">
      <div class="font-medium text-amber-900 dark:text-amber-200">{{ name }} was changed elsewhere while you were editing.</div>
      <div class="mt-2 flex flex-wrap gap-2">
        <button class="btn btn-sm" @click="showTheirs = !showTheirs">{{ showTheirs ? 'Hide' : 'Show' }} their version</button>
        <button class="btn btn-sm" @click="takeTheirs">Use theirs (drop mine)</button>
        <button class="btn btn-sm btn-danger" @click="overwrite">Overwrite with mine</button>
      </div>
      <div v-if="showTheirs" class="mt-2 overflow-hidden rounded-lg border border-zinc-200 dark:border-zinc-800">
        <DiffView :original="draft" :modified="conflict.theirs" :lang="'yaml'" height="16rem" />
      </div>
    </div>

    <div v-show="loaded" ref="el" class="min-h-[22rem] overflow-hidden" style="height: calc(100vh - 25rem)" />

    <div v-if="loaded" class="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-b-xl border-t border-zinc-200 px-4 py-2 text-xs dark:border-zinc-800">
      <span v-if="check === 'checking'" class="muted">Checking with docker compose…</span>
      <span v-else-if="check === 'ok'" class="text-emerald-700 dark:text-emerald-400">✓ valid</span>
      <button v-else-if="check === 'error' && vError" class="min-w-0 text-left text-red-600 hover:underline dark:text-red-400" @click="vError?.line && ed?.goto(vError.line)">
        ✕ <template v-if="vError.line">line {{ vError.line }}: </template>{{ vError.message }}
      </button>
      <span v-else class="muted">Edited · not checked yet</span>
      <span class="muted ml-auto">
        <template v-if="current?.mod_time">saved {{ ago(current.mod_time) }} · </template>Ctrl+S to save · Ctrl+F to search
      </span>
    </div>

    <Modal v-if="preview" :title="`Save ${name}?`" wide @close="preview = false">
      <div class="border-b border-zinc-200 px-4 py-2 text-xs dark:border-zinc-800">
        <span v-if="check === 'error'" class="text-red-600 dark:text-red-400">The file doesn't validate: saving will be refused. {{ vError?.message }}</span>
        <span v-else class="muted">Your changes. The previous version stays in History.</span>
      </div>
      <DiffView :original="original" :modified="draft" :lang="'yaml'" height="50vh" />
      <template #footer>
        <button class="btn" @click="preview = false">Cancel</button>
        <button class="btn" :disabled="saving" @click="save(false)">Save</button>
        <button class="btn btn-primary" :disabled="saving || stack.self" :title="stack.self ? 'Apply Dockira’s own stack from the host' : 'Save, then docker compose up -d'" @click="save(true)">Save &amp; apply</button>
      </template>
    </Modal>
  </div>
</template>
