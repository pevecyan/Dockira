<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, type ChannelView, type NotificationsOverview, type Rule } from '../api'
import { ago, dateTime } from '../format'
import { ask, state, toast } from '../state'
import ChannelDialog from '../components/ChannelDialog.vue'
import RuleDialog from '../components/RuleDialog.vue'

const data = ref<NotificationsOverview | null>(null)
const editChannel = ref<ChannelView | null | undefined>(undefined) // undefined: closed, null: new
const editRule = ref<Rule | null | undefined>(undefined)
const testing = ref<number | null>(null)

async function load() {
  try {
    data.value = await api.notifications()
  } catch (e: any) {
    toast('bad', e.message)
  }
}
onMounted(load)

async function test(c: ChannelView) {
  testing.value = c.id
  try {
    const r = await api.testChannel(c.id)
    r.ok ? toast('ok', `Test sent to ${c.name}`) : toast('bad', `${c.name}: ${r.error}`)
    load()
  } catch (e: any) {
    toast('bad', e.message)
  } finally {
    testing.value = null
  }
}

async function removeChannel(c: ChannelView) {
  if (!(await ask({ title: `Remove ${c.name}?`, text: 'Its rules are removed too.', confirm: 'Remove', danger: true }))) return
  try {
    await api.deleteChannel(c.id)
    load()
  } catch (e: any) {
    toast('bad', e.message)
  }
}

async function removeRule(r: Rule) {
  if (!(await ask({ title: 'Remove this rule?', text: 'Its events stop going to that channel.', confirm: 'Remove', danger: true }))) return
  try {
    await api.deleteRule(r.id)
    load()
  } catch (e: any) {
    toast('bad', e.message)
  }
}

const channelName = (id: number) => data.value?.channels.find((c) => c.id === id)?.name ?? `#${id}`
const describe = (t: string) => data.value?.event_types.find((e) => e.type === t)?.description ?? t
const icons: Record<string, string> = { ntfy: '🔔', gotify: '📣', telegram: '✈️', webhook: '🔗' }
const channelTarget = (c: ChannelView) => {
  const cfg = (c.config ?? {}) as Record<string, any>
  return c.type === 'ntfy' ? `${cfg.server ?? 'https://ntfy.sh'}/${cfg.topic ?? ''}` : c.type === 'gotify' ? cfg.server : c.type === 'telegram' ? `chat ${cfg.chat_id}` : cfg.preset ?? 'json'
}
const noRules = computed(() => data.value && data.value.channels.length && !data.value.rules.length)

async function quickRule() {
  const ch = data.value?.channels[0]
  if (!ch) return
  try {
    await api.createRule({ id: 0, channel_id: ch.id, events: [], stacks: [], min_severity: 'info', dedupe_minutes: 60 })
    toast('ok', `Everything now goes to ${ch.name}`)
    load()
  } catch (e: any) {
    toast('bad', e.message)
  }
}
</script>

<template>
  <div class="space-y-4">
    <div class="card">
      <div class="card-h"><span>Channels <span class="font-normal text-zinc-500">({{ data?.channels.length ?? 0 }})</span></span>
        <button class="btn btn-sm btn-primary font-normal" :disabled="state.info?.read_only" @click="editChannel = null">Add channel</button>
      </div>
      <div v-if="data && !data.channels.length" class="px-4 py-8 text-center text-sm">
        <p class="font-medium">No channels yet.</p>
        <p class="muted mt-1">Add ntfy, Gotify, Telegram or a webhook to hear about updates, crashes and logins.</p>
      </div>
      <div v-else class="grid grid-cols-1 gap-3 p-4 sm:grid-cols-2 xl:grid-cols-3">
        <div v-for="c in data?.channels ?? []" :key="c.id" class="rounded-xl border border-zinc-200 p-3 dark:border-zinc-800" :class="c.enabled ? '' : 'opacity-60'">
          <div class="flex items-start gap-2">
            <span class="text-lg" aria-hidden="true">{{ icons[c.type] }}</span>
            <div class="min-w-0 flex-1">
              <div class="truncate font-medium">{{ c.name }}</div>
              <div class="muted truncate text-xs">{{ c.type }} · {{ channelTarget(c) }}</div>
            </div>
            <span v-if="!c.enabled" class="badge bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400">off</span>
          </div>
          <div class="mt-2 text-xs">
            <span v-if="c.last_error" class="text-red-600 dark:text-red-400" :title="c.last_error">✕ last delivery failed {{ ago(c.last_at) }}: {{ c.last_error }}</span>
            <span v-else-if="c.last_ok" class="text-emerald-700 dark:text-emerald-400">✓ delivered {{ ago(c.last_ok) }}</span>
            <span v-else class="muted">nothing sent yet</span>
          </div>
          <div class="mt-3 flex flex-wrap gap-1.5">
            <button class="btn btn-sm" :disabled="testing === c.id" @click="test(c)">{{ testing === c.id ? 'Sending…' : 'Send test' }}</button>
            <button class="btn btn-sm" :disabled="state.info?.read_only" @click="editChannel = c">Edit</button>
            <button class="btn btn-sm !text-red-600 dark:!text-red-400" :disabled="state.info?.read_only" @click="removeChannel(c)">Remove</button>
          </div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-h"><span>Rules <span class="font-normal text-zinc-500">· which events go where</span></span>
        <button class="btn btn-sm font-normal" :disabled="!data?.channels.length || state.info?.read_only" @click="editRule = null">Add rule</button>
      </div>
      <div v-if="noRules" class="flex flex-wrap items-center gap-3 border-b border-amber-200 bg-amber-50 px-4 py-3 text-sm dark:border-amber-900 dark:bg-amber-950/30">
        <span>No rules yet, so nothing is sent.</span>
        <button class="btn btn-sm btn-primary" @click="quickRule">Send everything to {{ data!.channels[0].name }}</button>
      </div>
      <div v-if="data && !data.rules.length && !noRules" class="muted px-4 py-6 text-center text-sm">Add a channel first, then rules decide what it gets.</div>
      <ul class="divide-y divide-zinc-100 text-sm dark:divide-zinc-800/70">
        <li v-for="r in data?.rules ?? []" :key="r.id" class="flex flex-wrap items-start gap-3 px-4 py-3">
          <div class="min-w-0 flex-1">
            <div>
              <span class="font-medium">{{ channelName(r.channel_id) }}</span>
              <span class="muted"> gets </span>
              <template v-if="!r.events.length || r.events.includes('*')">all events</template>
              <template v-else>
                <span v-for="e in r.events" :key="e" class="badge mr-1 mb-1 bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300" :title="e">{{ describe(e) }}</span>
              </template>
            </div>
            <div class="muted mt-0.5 text-xs">
              {{ r.min_severity }} and up · {{ r.stacks.length ? r.stacks.join(', ') : 'all stacks' }} ·
              {{ r.dedupe_minutes < 0 ? 'every repeat' : `repeats muted for ${r.dedupe_minutes || 60} min` }}
              <template v-if="r.quiet_hours"> · quiet {{ r.quiet_hours.start }}–{{ r.quiet_hours.end }}</template>
            </div>
          </div>
          <div class="flex gap-1.5">
            <button class="btn btn-sm" :disabled="state.info?.read_only" @click="editRule = r">Edit</button>
            <button class="btn btn-sm !text-red-600 dark:!text-red-400" :disabled="state.info?.read_only" @click="removeRule(r)">Remove</button>
          </div>
        </li>
      </ul>
    </div>

    <div class="card">
      <div class="card-h">Recent deliveries <button class="btn btn-sm font-normal" @click="load">Reload</button></div>
      <div v-if="!data?.log.length" class="muted px-4 py-6 text-center text-sm">Nothing sent yet.</div>
      <ul class="max-h-[24rem] divide-y divide-zinc-100 overflow-auto text-sm dark:divide-zinc-800/70">
        <li v-for="(l, i) in data?.log ?? []" :key="i" class="flex flex-wrap items-center gap-x-3 gap-y-0.5 px-4 py-1.5">
          <span :class="l.ok ? 'text-emerald-600' : 'text-red-600'">{{ l.ok ? '✓' : '✕' }}</span>
          <span class="muted w-36 text-xs" :title="dateTime(l.time)">{{ ago(l.time) }}</span>
          <span class="font-medium">{{ channelName(l.channel_id) }}</span>
          <span class="min-w-0 flex-1 truncate">{{ l.title || l.event }}</span>
          <span v-if="l.attempt > 1" class="muted text-xs">attempt {{ l.attempt }}</span>
          <span v-if="l.error" class="w-full text-xs break-words text-red-600 dark:text-red-400">{{ l.error }}</span>
        </li>
      </ul>
    </div>

    <ChannelDialog v-if="editChannel !== undefined" :channel="editChannel ?? undefined" @close="editChannel = undefined" @saved="editChannel = undefined; load()" />
    <RuleDialog
      v-if="editRule !== undefined && data"
      :rule="editRule ?? undefined"
      :channels="data.channels"
      :event-types="data.event_types"
      @close="editRule = undefined"
      @saved="editRule = undefined; load()"
    />
  </div>
</template>
