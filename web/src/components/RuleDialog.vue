<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { api, type ChannelView, type EventType, type Rule } from '../api'
import { state, toast } from '../state'
import Modal from './Modal.vue'

// Which events go to which channel.
const props = defineProps<{ rule?: Rule; channels: ChannelView[]; eventTypes: EventType[] }>()
const emit = defineEmits<{ close: []; saved: [] }>()

const form = reactive<Rule>(
  props.rule
    ? JSON.parse(JSON.stringify(props.rule))
    : { id: 0, channel_id: props.channels[0]?.id ?? 0, events: [], stacks: [], min_severity: 'warning', dedupe_minutes: 60 },
)
const quiet = ref(!!form.quiet_hours)
const qh = reactive({ start: form.quiet_hours?.start ?? '22:00', end: form.quiet_hours?.end ?? '07:00' })
const allEvents = computed({
  get: () => !form.events.length || form.events.includes('*'),
  set: (v: boolean) => (form.events = v ? [] : props.eventTypes.filter((e) => e.type !== 'test').map((e) => e.type)),
})
const allStacks = computed({
  get: () => !form.stacks.length,
  set: (v: boolean) => (form.stacks = v ? [] : state.stacks.filter((s) => !s.orphaned).map((s) => s.id)),
})
function toggle(list: string[], v: string) {
  const i = list.indexOf(v)
  i >= 0 ? list.splice(i, 1) : list.push(v)
}
const groups = computed(() => {
  const g = new Map<string, EventType[]>()
  for (const e of props.eventTypes) {
    if (e.type === 'test') continue
    const k = e.type.split('.')[0]
    g.set(k, [...(g.get(k) ?? []), e])
  }
  return [...g.entries()]
})
const busy = ref(false)
const error = ref('')

async function save() {
  busy.value = true
  error.value = ''
  const r: Rule = JSON.parse(JSON.stringify(form))
  r.quiet_hours = quiet.value ? { start: qh.start, end: qh.end } : undefined
  try {
    if (r.id) await api.updateRule(r.id, r)
    else await api.createRule(r)
    toast('ok', 'Rule saved')
    emit('saved')
  } catch (e: any) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Modal :title="rule ? 'Edit rule' : 'Add a rule'" wide @close="emit('close')">
    <form id="rule-form" class="space-y-4 px-4 py-4 text-sm" @submit.prevent="save">
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div>
          <label class="label" for="r-ch">Send to</label>
          <select id="r-ch" v-model="form.channel_id" class="input">
            <option v-for="c in channels" :key="c.id" :value="c.id">{{ c.name }} ({{ c.type }})</option>
          </select>
        </div>
        <div>
          <label class="label" for="r-sev">At least</label>
          <select id="r-sev" v-model="form.min_severity" class="input">
            <option value="info">info (everything)</option>
            <option value="warning">warning</option>
            <option value="error">error</option>
          </select>
        </div>
      </div>

      <div>
        <div class="label">Events</div>
        <label class="mb-2 flex items-center gap-2"><input v-model="allEvents" type="checkbox" class="accent-emerald-600" />all events</label>
        <div v-if="!allEvents" class="grid grid-cols-1 gap-x-4 gap-y-3 sm:grid-cols-2">
          <div v-for="[g, list] in groups" :key="g">
            <div class="mb-1 text-[11px] font-semibold uppercase tracking-wider text-zinc-400">{{ g }}</div>
            <label v-for="e in list" :key="e.type" class="flex items-start gap-2 py-0.5" :title="e.type">
              <input type="checkbox" class="mt-0.5 accent-emerald-600" :checked="form.events.includes(e.type)" @change="toggle(form.events, e.type)" />
              <span>{{ e.description }} <span class="muted text-[11px]">{{ e.severity }}</span></span>
            </label>
          </div>
        </div>
      </div>

      <div>
        <div class="label">Stacks</div>
        <label class="mb-2 flex items-center gap-2"><input v-model="allStacks" type="checkbox" class="accent-emerald-600" />all stacks</label>
        <div v-if="!allStacks" class="flex flex-wrap gap-x-4 gap-y-1">
          <label v-for="s in state.stacks.filter((s) => !s.orphaned)" :key="s.id" class="flex items-center gap-2">
            <input type="checkbox" class="accent-emerald-600" :checked="form.stacks.includes(s.id)" @change="toggle(form.stacks, s.id)" />{{ s.id }}
          </label>
        </div>
        <p class="muted mt-1 text-xs">Events not tied to a stack (logins, disk, failed checks) always pass this filter.</p>
      </div>

      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div>
          <label class="label" for="r-dd">Don't repeat the same message within (minutes)</label>
          <input id="r-dd" v-model.number="form.dedupe_minutes" type="number" min="-1" class="input" />
          <p class="muted mt-1 text-xs">0 = 60 minutes, −1 = always send.</p>
        </div>
        <div>
          <label class="flex items-center gap-2"><input v-model="quiet" type="checkbox" class="accent-emerald-600" />Quiet hours</label>
          <div v-if="quiet" class="mt-1.5 flex items-center gap-2">
            <input v-model="qh.start" type="time" class="input !w-32" aria-label="Quiet from" />
            <span class="muted">to</span>
            <input v-model="qh.end" type="time" class="input !w-32" aria-label="Quiet until" />
          </div>
          <p class="muted mt-1 text-xs">Security messages (logins, lockouts) always get through.</p>
        </div>
      </div>
      <div v-if="error" class="rounded-lg border border-red-300 bg-red-50 px-3 py-2 text-red-800 dark:border-red-900 dark:bg-red-950/50 dark:text-red-200">{{ error }}</div>
    </form>
    <template #footer>
      <button class="btn" @click="emit('close')">Cancel</button>
      <button class="btn btn-primary" type="submit" form="rule-form" :disabled="busy || !form.channel_id">Save</button>
    </template>
  </Modal>
</template>
