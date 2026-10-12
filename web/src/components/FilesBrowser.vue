<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { onBeforeRouteLeave, useRoute, useRouter } from 'vue-router'
import { api, ApiError, urls, type FolderEntry, type FolderListing, type Stack, type ValidationError } from '../api'
import { createEditor, langFor } from '../editor/cm'
import { ago, bytes, dateTime } from '../format'
import { ask, confirmPassword, promptText, runAction, state, toast } from '../state'
import DiffView from './DiffView.vue'
import Icon from './Icon.vue'
import Modal from './Modal.vue'

// The whole stack folder: browse, view and edit text files, upload, download, create, delete.
const props = defineProps<{ stack: Stack }>()
const route = useRoute()
const router = useRouter()

const cwd = ref(String(route.query.dir ?? ''))
const listing = ref<FolderListing | null>(null)
const listError = ref('')

// the open file
const path = ref('')
const original = ref('')
const draft = ref('')
const sha = ref('')
const size = ref(0)
const modTime = ref('')
const isNew = ref(false)
const fileError = ref<{ kind: 'binary' | 'large' | 'other'; message: string } | null>(null)
const loading = ref(false)
const vError = ref<ValidationError | null>(null)
const preview = ref(false)
const saving = ref(false)
const dirty = computed(() => !!path.value && !fileError.value && draft.value !== original.value)
const readOnly = computed(() => !!state.info?.read_only)
const isCompose = computed(() => props.stack.files.some((f) => f.split('/').pop() === path.value))

const el = ref<HTMLElement | null>(null)
let ed: ReturnType<typeof createEditor> | null = null

async function loadDir(dir = cwd.value) {
  listError.value = ''
  try {
    listing.value = await api.tree(props.stack.id, dir)
    cwd.value = listing.value.path
    syncQuery()
  } catch (e: any) {
    listError.value = e.message
  }
}

function syncQuery() {
  router.replace({ query: { ...route.query, tab: 'files', dir: cwd.value || undefined, file: path.value || undefined } })
}

async function guard(): Promise<boolean> {
  return !dirty.value || ask({ title: 'Discard unsaved changes?', text: `Your changes to ${path.value} are lost.`, confirm: 'Discard', danger: true })
}

async function openDir(dir: string) {
  if (!(await guard())) return
  closeFile()
  await loadDir(dir)
}

function closeFile() {
  ed?.destroy()
  ed = null
  path.value = ''
  fileError.value = null
  vError.value = null
  syncQuery()
}

async function openFile(p: string, fresh = false) {
  if (p !== path.value && !(await guard())) return
  ed?.destroy()
  ed = null
  path.value = p
  isNew.value = fresh
  fileError.value = null
  vError.value = null
  original.value = draft.value = sha.value = modTime.value = ''
  size.value = 0
  syncQuery()
  if (!fresh) {
    loading.value = true
    try {
      const f = await api.readAny(props.stack.id, p)
      original.value = draft.value = f.content
      sha.value = f.sha256
      size.value = f.size
      modTime.value = f.mod_time ?? ''
    } catch (e: any) {
      if (e instanceof ApiError && e.status === 415) fileError.value = { kind: 'binary', message: 'A binary file: it can be downloaded but not shown here.' }
      else if (e instanceof ApiError && e.status === 413) fileError.value = { kind: 'large', message: e.message }
      else if (e instanceof ApiError && e.status === 403 && e.body?.confirm) fileError.value = { kind: 'other', message: 'Password confirmation cancelled.' }
      else fileError.value = { kind: 'other', message: e.message }
      const entry = listing.value?.entries.find((x) => x.path === p)
      size.value = entry?.size ?? 0
      return
    } finally {
      loading.value = false
    }
  }
  await nextTick()
  if (!el.value) return
  ed = createEditor(el.value, {
    doc: draft.value,
    lang: langFor(p),
    readOnly: readOnly.value,
    onChange: (d) => {
      draft.value = d
      vError.value = null
      ed?.mark()
    },
    onSave: () => dirty.value && (preview.value = true),
  })
}

async function save(apply = false, base = sha.value) {
  saving.value = true
  try {
    const r = await api.saveAny(props.stack.id, path.value, draft.value, base, apply)
    original.value = draft.value
    sha.value = r.sha256
    isNew.value = false
    preview.value = false
    toast('ok', `${path.value} saved`)
    if (r.job) state.jobs[r.job.id] = r.job
    loadDir()
  } catch (e: any) {
    preview.value = false
    if (e instanceof ApiError && e.status === 409) {
      if (await ask({ title: `${path.value} changed elsewhere`, text: 'Someone changed the file since you opened it. Overwrite their version with yours? Theirs stays in History.', confirm: 'Overwrite', danger: true }))
        await save(apply, e.body?.current_sha256 ?? '')
    } else if (e instanceof ApiError && e.status === 422) {
      vError.value = { message: e.message, line: e.body?.line, column: e.body?.column }
      ed?.mark(e.body?.line, e.body?.column, e.message)
      toast('bad', 'Not saved: ' + e.message)
    } else {
      toast('bad', e.message)
    }
  } finally {
    saving.value = false
  }
}

async function saveAndRestart() {
  await save(false)
  if (!dirty.value) await runAction(props.stack, 'restart')
}

async function revert() {
  if (await ask({ title: 'Discard your changes?', text: 'The editor goes back to the saved file.', confirm: 'Discard', danger: true })) {
    draft.value = original.value
    ed?.setDoc(original.value)
  }
}

const join = (name: string) => (cwd.value ? `${cwd.value}/${name}` : name)

async function newFile() {
  const name = await promptText({ title: 'New file', label: `Name (in ${cwd.value || 'the stack folder'})`, placeholder: 'config.yaml' })
  if (!name) return
  if (listing.value?.entries.some((e) => e.path === join(name))) return toast('warn', `${name} exists already`)
  if (await confirmPassword()) openFile(join(name), true)
}

async function newFolder() {
  const name = await promptText({ title: 'New folder', label: `Name (in ${cwd.value || 'the stack folder'})`, placeholder: 'config' })
  if (!name) return
  try {
    await api.createFolder(props.stack.id, join(name))
    loadDir()
  } catch (e: any) {
    toast('bad', e.message)
  }
}

async function remove(e: FolderEntry) {
  const folder = e.type === 'dir'
  if (!(await ask({ title: `Delete ${e.name}?`, text: folder ? 'Only empty folders can be deleted.' : 'The file is deleted. Its last version stays in History, where it can be restored.', confirm: 'Delete', danger: true }))) return
  try {
    await api.deleteAny(props.stack.id, e.path)
    if (path.value === e.path) closeFile()
    toast('ok', `${e.name} deleted`)
    loadDir()
  } catch (err: any) {
    toast('bad', err.message)
  }
}

const upInput = ref<HTMLInputElement | null>(null)
async function upload(ev: Event) {
  const files = Array.from((ev.target as HTMLInputElement).files ?? [])
  ;(ev.target as HTMLInputElement).value = ''
  if (!files.length || !(await confirmPassword())) return
  for (const f of files) {
    if (listing.value?.entries.some((e) => e.path === join(f.name)) && !(await ask({ title: `Replace ${f.name}?`, text: 'A file with this name exists in this folder.', confirm: 'Replace', danger: true })))
      continue
    const res = await fetch(urls.upload(props.stack.id, join(f.name)), { method: 'PUT', body: f, credentials: 'same-origin' })
    if (res.ok) toast('ok', `${f.name} uploaded`)
    else toast('bad', `${f.name}: ${(await res.json().catch(() => null))?.detail ?? res.statusText}`)
  }
  loadDir()
}

async function download(p: string) {
  if (!(await confirmPassword())) return
  const a = document.createElement('a')
  a.href = urls.download(props.stack.id, p)
  a.download = p.split('/').pop()!
  a.click()
}

const crumbs = computed(() => {
  const parts = cwd.value ? cwd.value.split('/') : []
  return parts.map((name, i) => ({ name, path: parts.slice(0, i + 1).join('/') }))
})
const parent = computed(() => (cwd.value.includes('/') ? cwd.value.slice(0, cwd.value.lastIndexOf('/')) : ''))

onMounted(async () => {
  await loadDir()
  if (route.query.file) openFile(String(route.query.file))
})
onBeforeUnmount(() => ed?.destroy())
watch(readOnly, (r) => ed?.setReadOnly(r))
onBeforeRouteLeave(async () => await guard())
function beforeUnload(e: BeforeUnloadEvent) {
  if (dirty.value) e.preventDefault()
}
window.addEventListener('beforeunload', beforeUnload)
onBeforeUnmount(() => window.removeEventListener('beforeunload', beforeUnload))

const icon = (e: FolderEntry) => (e.type === 'dir' ? 'folder' : e.type === 'symlink' ? 'link' : 'file')
</script>

<template>
  <div class="grid grid-cols-1 gap-4 lg:grid-cols-[minmax(0,22rem)_minmax(0,1fr)]">
    <!-- folder -->
    <div class="card self-start">
      <div class="card-h flex-wrap">
        <nav class="flex min-w-0 flex-wrap items-center gap-1 font-normal" aria-label="Folder">
          <button class="font-semibold hover:underline" @click="openDir('')">{{ stack.name }}</button>
          <template v-for="c in crumbs" :key="c.path">
            <span class="text-zinc-400">/</span>
            <button class="truncate hover:underline" @click="openDir(c.path)">{{ c.name }}</button>
          </template>
        </nav>
        <div class="flex gap-1 font-normal">
          <button class="btn btn-sm !px-2" title="New file" aria-label="New file" :disabled="readOnly" @click="newFile"><Icon name="plus" :size="14" /><Icon name="file" :size="14" /></button>
          <button class="btn btn-sm !px-2" title="New folder" aria-label="New folder" :disabled="readOnly" @click="newFolder"><Icon name="plus" :size="14" /><Icon name="folder" :size="14" /></button>
          <button class="btn btn-sm !px-2" title="Upload files" aria-label="Upload files" :disabled="readOnly" @click="upInput?.click()"><Icon name="upload" :size="14" /></button>
          <button class="btn btn-sm !px-2" title="Refresh" aria-label="Refresh" @click="loadDir()"><Icon name="refresh" :size="14" /></button>
          <input ref="upInput" type="file" multiple class="hidden" @change="upload" />
        </div>
      </div>
      <div v-if="listError" class="px-4 py-3 text-sm text-red-600 dark:text-red-400">{{ listError }}</div>
      <ul v-else class="max-h-[70vh] overflow-auto py-1 text-sm">
        <li v-if="cwd">
          <button class="flex w-full items-center gap-2 px-4 py-1.5 text-left text-zinc-500 hover:bg-zinc-50 dark:hover:bg-zinc-800/40" @click="openDir(parent)">
            <Icon name="folder" :size="15" /> ..
          </button>
        </li>
        <li v-for="e in listing?.entries ?? []" :key="e.path" class="group relative">
          <button
            class="flex w-full items-center gap-2 px-4 py-1.5 text-left hover:bg-zinc-50 disabled:cursor-not-allowed disabled:opacity-50 dark:hover:bg-zinc-800/40"
            :class="path === e.path ? 'bg-emerald-50/70 text-emerald-800 dark:bg-emerald-950/40 dark:text-emerald-300' : ''"
            :disabled="!e.readable"
            :title="e.locked ? 'Dockira keeps its own secrets out of reach here' : e.outside ? `links outside the stack folder (${e.target})` : e.target ? `→ ${e.target}` : e.name"
            @click="e.type === 'dir' ? openDir(e.path) : openFile(e.path)"
          >
            <Icon :name="icon(e)" :size="15" class="shrink-0" :class="e.type === 'dir' ? 'text-sky-600 dark:text-sky-400' : 'text-zinc-400'" />
            <span class="min-w-0 flex-1 truncate">{{ e.name }}</span>
            <span v-if="e.locked" class="text-[11px] text-zinc-400">locked</span>
            <span v-else-if="e.type !== 'dir'" class="shrink-0 text-[11px] text-zinc-400 tabular-nums group-hover:invisible">{{ bytes(e.size, 0) }}</span>
          </button>
          <button
            v-if="e.readable && !readOnly && !stack.files.some((f) => f.split('/').pop() === e.path)"
            class="absolute top-1/2 right-2 hidden -translate-y-1/2 rounded p-1 text-zinc-400 group-hover:block hover:bg-zinc-100 hover:text-red-600 dark:hover:bg-zinc-800"
            :title="`Delete ${e.name}`"
            :aria-label="`Delete ${e.name}`"
            @click.stop="remove(e)"
          ><Icon name="trash" :size="13" /></button>
        </li>
        <li v-if="listing && !listing.entries.length" class="muted px-4 py-4 text-center text-xs">Empty folder.</li>
        <li v-if="listing?.truncated" class="muted px-4 py-2 text-xs">Only the first 2000 entries are listed.</li>
      </ul>
    </div>

    <!-- file -->
    <div class="card flex min-h-[20rem] flex-col self-start overflow-hidden">
      <div v-if="!path" class="muted flex flex-1 items-center justify-center px-4 py-16 text-center text-sm">
        Pick a file to view or edit it.<br />Files other than the compose files ask for your password first: folders like this one often hold keys.
      </div>
      <template v-else>
        <div class="card-h flex-wrap">
          <span class="flex min-w-0 items-center gap-2">
            <span class="truncate font-mono text-sm">{{ path }}</span>
            <span v-if="dirty" class="h-2 w-2 shrink-0 rounded-full bg-amber-500" title="Unsaved changes" />
            <span v-if="isNew" class="badge bg-sky-100 text-sky-800 dark:bg-sky-950 dark:text-sky-300">new</span>
          </span>
          <div class="flex flex-wrap gap-1.5 font-normal">
            <button v-if="!isNew" class="btn btn-sm" @click="download(path)">Download</button>
            <button v-if="!fileError" class="btn btn-sm" :disabled="!dirty" @click="revert">Revert</button>
            <button v-if="!fileError" class="btn btn-sm btn-primary" :disabled="!dirty || saving || readOnly" title="Save (Ctrl+S)" @click="preview = true">Save…</button>
          </div>
        </div>
        <div v-if="loading" class="muted px-4 py-10 text-center text-sm">Loading…</div>
        <div v-else-if="fileError" class="flex flex-1 flex-col items-center justify-center gap-3 px-4 py-14 text-center text-sm">
          <Icon name="file" :size="28" class="text-zinc-400" />
          <p>{{ fileError.message }}</p>
          <p v-if="size" class="muted text-xs">{{ bytes(size) }}</p>
          <button v-if="fileError.kind !== 'other'" class="btn btn-sm" @click="download(path)">Download</button>
          <button v-else class="btn btn-sm" @click="openFile(path)">Try again</button>
        </div>
        <div v-show="!loading && !fileError" ref="el" class="min-h-[22rem] overflow-hidden" style="height: calc(100vh - 24rem)" />
        <div v-if="!loading && !fileError" class="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-zinc-200 px-4 py-2 text-xs dark:border-zinc-800">
          <button v-if="vError" class="text-left text-red-600 hover:underline dark:text-red-400" @click="vError?.line && ed?.goto(vError.line)">
            ✕ <template v-if="vError.line">line {{ vError.line }}: </template>{{ vError.message }}
          </button>
          <span v-else class="muted">{{ langFor(path) }}</span>
          <span class="muted ml-auto" :title="dateTime(modTime)"><template v-if="modTime">saved {{ ago(modTime) }} · </template>{{ bytes(size) }} · Ctrl+S to save</span>
        </div>
      </template>
    </div>

    <Modal v-if="preview" :title="`Save ${path}?`" wide @close="preview = false">
      <div class="muted border-b border-zinc-200 px-4 py-2 text-xs dark:border-zinc-800">
        {{ isNew ? 'A new file.' : 'Your changes.' }} The previous version stays in History.
        <template v-if="isCompose"> Compose files are checked with docker compose before saving.</template>
        <template v-else-if="/\.(ya?ml|json)$/i.test(path)"> The syntax is checked before saving.</template>
      </div>
      <DiffView :original="original" :modified="draft" :lang="langFor(path)" height="50vh" />
      <template #footer>
        <button class="btn" @click="preview = false">Cancel</button>
        <button class="btn" :class="isCompose ? '' : 'btn-primary'" :disabled="saving" @click="save(false)">Save</button>
        <button v-if="!stack.self && isCompose" class="btn btn-primary" :disabled="saving" title="Save, then docker compose up -d" @click="save(true)">Save &amp; apply</button>
        <button v-else-if="!stack.self && stack.running" class="btn" :disabled="saving" title="Save, then restart the stack so its containers read the file again" @click="saveAndRestart">Save &amp; restart</button>
      </template>
    </Modal>
  </div>
</template>
