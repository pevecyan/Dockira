<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { api, type ChannelBody, type ChannelView } from '../api'
import { toast } from '../state'
import Modal from './Modal.vue'

// Add or edit a notification channel. Secrets are write-only: the server never sends them,
// and an empty secret field keeps the stored value.
const props = defineProps<{ channel?: ChannelView }>()
const emit = defineEmits<{ close: []; saved: [] }>()

type Kind = ChannelBody['type']
const kinds: { v: Kind; label: string; hint: string }[] = [
  { v: 'ntfy', label: 'ntfy', hint: 'Push to your phone via ntfy.sh or your own ntfy server' },
  { v: 'gotify', label: 'Gotify', hint: 'Your Gotify server' },
  { v: 'telegram', label: 'Telegram', hint: 'A bot posting into a chat' },
  { v: 'webhook', label: 'Webhook', hint: 'Discord, Slack, Home Assistant, anything with a URL' },
]

const edit = !!props.channel
const form = reactive<ChannelBody>({
  type: (props.channel?.type as Kind) ?? 'ntfy',
  name: props.channel?.name ?? '',
  enabled: props.channel?.enabled ?? true,
  config: JSON.parse(JSON.stringify((props.channel?.config as Record<string, any>) ?? { server: 'https://ntfy.sh' })),
})
const set = new Set(props.channel?.secrets_set ?? [])
const headers = ref<{ k: string; v: string }[]>(Object.keys(form.config.headers ?? {}).map((k) => ({ k, v: '' })))
const busy = ref(false)
const error = ref('')

function pick(t: Kind) {
  form.type = t
  form.config = t === 'ntfy' ? { server: 'https://ntfy.sh' } : t === 'gotify' ? { server: '' } : t === 'webhook' ? { preset: 'json', method: 'POST' } : {}
  headers.value = []
}

const secretPh = (field: string) => (set.has(field) ? '•••••  (set — leave empty to keep)' : '')

async function save() {
  busy.value = true
  error.value = ''
  const body: ChannelBody = JSON.parse(JSON.stringify(form))
  if (form.type === 'webhook') {
    body.config.headers = Object.fromEntries(headers.value.filter((h) => h.k.trim()).map((h) => [h.k.trim(), h.v]))
    if (!Object.keys(body.config.headers).length) delete body.config.headers
  }
  if (form.type === 'ntfy' && typeof body.config.tags === 'string') body.config.tags = body.config.tags.split(',').map((t: string) => t.trim()).filter(Boolean)
  if (form.type === 'telegram' && body.config.thread_id) body.config.thread_id = Number(body.config.thread_id)
  try {
    if (edit) await api.updateChannel(props.channel!.id, body)
    else await api.createChannel(body)
    toast('ok', `Channel ${body.name} saved`)
    emit('saved')
  } catch (e: any) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

const ntfyTags = computed({
  get: () => (Array.isArray(form.config.tags) ? form.config.tags.join(', ') : form.config.tags ?? ''),
  set: (v: string) => (form.config.tags = v),
})
</script>

<template>
  <Modal :title="edit ? `Edit ${channel!.name}` : 'Add a notification channel'" wide @close="emit('close')">
    <form id="channel-form" class="space-y-4 px-4 py-4 text-sm" @submit.prevent="save">
      <div v-if="!edit" class="grid grid-cols-1 gap-2 sm:grid-cols-4">
        <button
          v-for="k in kinds"
          :key="k.v"
          type="button"
          class="rounded-lg border px-3 py-2 text-left transition"
          :class="form.type === k.v ? 'border-emerald-500 bg-emerald-50 dark:bg-emerald-950/40' : 'border-zinc-200 hover:border-zinc-400 dark:border-zinc-700'"
          @click="pick(k.v)"
        >
          <div class="font-medium">{{ k.label }}</div>
          <div class="muted text-xs">{{ k.hint }}</div>
        </button>
      </div>
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_auto]">
        <div>
          <label class="label" for="ch-name">Name</label>
          <input id="ch-name" v-model="form.name" class="input" required maxlength="80" placeholder="e.g. My phone" />
        </div>
        <label class="flex items-center gap-2 self-end pb-2"><input v-model="form.enabled" type="checkbox" class="accent-emerald-600" />enabled</label>
      </div>

      <template v-if="form.type === 'ntfy'">
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div><label class="label">Server</label><input v-model="form.config.server" class="input" placeholder="https://ntfy.sh" /></div>
          <div><label class="label">Topic</label><input v-model="form.config.topic" class="input" required placeholder="a long, unguessable name" /></div>
          <div><label class="label">Access token (optional)</label><input v-model="form.config.token" type="password" class="input" :placeholder="secretPh('token')" autocomplete="off" /></div>
          <div><label class="label">Tags (optional)</label><input v-model="ntfyTags" class="input" placeholder="docker, server" /></div>
          <div><label class="label">Username (optional)</label><input v-model="form.config.username" class="input" autocomplete="off" /></div>
          <div><label class="label">Password (optional)</label><input v-model="form.config.password" type="password" class="input" :placeholder="secretPh('password')" autocomplete="off" /></div>
        </div>
      </template>

      <template v-else-if="form.type === 'gotify'">
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div><label class="label">Server</label><input v-model="form.config.server" class="input" required placeholder="https://gotify.example.com" /></div>
          <div><label class="label">Application token</label><input v-model="form.config.token" type="password" class="input" :placeholder="secretPh('token')" :required="!set.has('token')" autocomplete="off" /></div>
        </div>
      </template>

      <template v-else-if="form.type === 'telegram'">
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div><label class="label">Bot token</label><input v-model="form.config.bot_token" type="password" class="input" :placeholder="secretPh('bot_token') || '123456:ABC…'" :required="!set.has('bot_token')" autocomplete="off" /></div>
          <div><label class="label">Chat ID</label><input v-model="form.config.chat_id" class="input" required placeholder="-1001234567890 or @channel" /></div>
          <div><label class="label">Topic (thread) ID, optional</label><input v-model="form.config.thread_id" class="input" inputmode="numeric" /></div>
          <label class="flex items-center gap-2 self-end pb-2"><input v-model="form.config.silent_info" type="checkbox" class="accent-emerald-600" />info messages without sound</label>
        </div>
        <p class="muted text-xs">Create a bot with @BotFather, add it to the chat, and use the chat's ID.</p>
      </template>

      <template v-else>
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_8rem_9rem]">
          <div><label class="label">URL</label><input v-model="form.config.url" type="password" class="input font-mono" :placeholder="secretPh('url') || 'https://…'" :required="!set.has('url')" autocomplete="off" /></div>
          <div>
            <label class="label">Method</label>
            <select v-model="form.config.method" class="input"><option>POST</option><option>PUT</option><option>PATCH</option></select>
          </div>
          <div>
            <label class="label">Format</label>
            <select v-model="form.config.preset" class="input">
              <option value="json">JSON event</option>
              <option value="discord">Discord</option>
              <option value="slack">Slack</option>
              <option value="custom">custom template</option>
            </select>
          </div>
        </div>
        <p class="muted -mt-2 text-xs">The URL is treated as a secret (Discord and Slack URLs contain the token).</p>
        <div v-if="form.config.preset === 'custom'">
          <label class="label">Body template (Go text/template over the event: .Title .Message .Type .Severity .Stack .URL .Time)</label>
          <textarea v-model="form.config.template" rows="4" class="input font-mono !text-xs" placeholder='{"text": {{json .Title}}}' />
        </div>
        <div>
          <div class="label">Headers (values are secret)</div>
          <div v-for="(h, i) in headers" :key="i" class="mb-1.5 flex gap-2">
            <input v-model="h.k" class="input !w-48 font-mono !text-xs" placeholder="Authorization" aria-label="Header name" />
            <input v-model="h.v" type="password" class="input font-mono !text-xs" :placeholder="set.has('headers') && h.k ? '•••••  (leave empty to keep)' : 'value'" aria-label="Header value" autocomplete="off" />
            <button type="button" class="btn btn-sm" aria-label="Remove header" @click="headers.splice(i, 1)">×</button>
          </div>
          <button type="button" class="btn btn-sm" @click="headers.push({ k: '', v: '' })">+ header</button>
        </div>
      </template>

      <div v-if="error" class="rounded-lg border border-red-300 bg-red-50 px-3 py-2 text-red-800 dark:border-red-900 dark:bg-red-950/50 dark:text-red-200">{{ error }}</div>
    </form>
    <template #footer>
      <span class="muted mr-auto text-xs">Saving asks for your password (the channel holds secrets).</span>
      <button class="btn" @click="emit('close')">Cancel</button>
      <button class="btn btn-primary" type="submit" form="channel-form" :disabled="busy">Save</button>
    </template>
  </Modal>
</template>
