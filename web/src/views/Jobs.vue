<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api, type Job } from '../api'
import { ago, duration, enc } from '../format'
import { activeJobs, state, toast } from '../state'
import JobOutput from '../components/JobOutput.vue'
import StatusBadge from '../components/StatusBadge.vue'

const route = useRoute()
const jobs = ref<Job[]>([])
const more = ref(true)
const selected = ref<number | null>(route.query.id ? Number(route.query.id) : null)
const filter = ref<'all' | 'failed' | 'auto'>('all')

async function load(append = false) {
  try {
    const before = append && jobs.value.length ? jobs.value[jobs.value.length - 1].id : undefined
    const l = await api.jobs({ limit: 50, before })
    jobs.value = append ? [...jobs.value, ...l.jobs] : l.jobs
    more.value = l.jobs.length === 50
    if (!selected.value && jobs.value.length) selected.value = jobs.value[0].id
  } catch (e: any) {
    toast('bad', e.message)
  }
}
load()
// live: refresh the list when a job starts or ends
watch(() => Object.values(state.jobs).map((j) => j.id + j.state).join(), () => load())

const shown = computed(() =>
  jobs.value.map((j) => state.jobs[j.id] ?? j).filter((j) => filter.value === 'all' || (filter.value === 'failed' ? j.state === 'failed' : j.trigger !== 'user')),
)
const took = (j: Job) => (j.started_at && j.ended_at ? duration((Date.parse(j.ended_at) - Date.parse(j.started_at)) / 1000) : '')
</script>

<template>
  <div class="grid grid-cols-1 gap-4 lg:grid-cols-[minmax(0,26rem)_minmax(0,1fr)]">
    <div class="card self-start">
      <div class="card-h">
        <span>Jobs <span class="font-normal text-zinc-500">({{ activeJobs().length }} running)</span></span>
        <select v-model="filter" class="input !w-auto !py-1 text-xs font-normal" aria-label="Filter jobs">
          <option value="all">all</option>
          <option value="failed">failed</option>
          <option value="auto">automatic</option>
        </select>
      </div>
      <div v-if="!shown.length" class="muted px-4 py-8 text-center text-sm">No jobs yet.</div>
      <ul class="max-h-[70vh] divide-y divide-zinc-100 overflow-auto dark:divide-zinc-800/70">
        <li v-for="j in shown" :key="j.id">
          <button
            class="w-full px-4 py-2 text-left text-sm hover:bg-zinc-50 dark:hover:bg-zinc-800/30"
            :class="selected === j.id ? 'bg-emerald-50/60 dark:bg-emerald-950/30' : ''"
            @click="selected = j.id"
          >
            <div class="flex items-center gap-2">
              <StatusBadge :status="j.state === 'running' ? 'running_job' : j.state" :text="j.state" />
              <span class="min-w-0 flex-1 truncate font-medium">{{ j.title }}</span>
            </div>
            <div class="muted mt-0.5 text-xs">#{{ j.id }} · {{ j.trigger }} · {{ ago(j.created_at) }}<template v-if="took(j)"> · {{ took(j) }}</template></div>
          </button>
        </li>
      </ul>
      <div v-if="more && shown.length" class="border-t border-zinc-200 px-4 py-2 text-center dark:border-zinc-800">
        <button class="btn btn-sm" @click="load(true)">Older jobs</button>
      </div>
    </div>
    <div v-if="selected" class="card self-start overflow-hidden">
      <div class="card-h">
        <span class="truncate">{{ (state.jobs[selected] ?? jobs.find((j) => j.id === selected))?.title ?? `Job #${selected}` }}</span>
        <RouterLink
          v-if="(state.jobs[selected] ?? jobs.find((j) => j.id === selected))?.stack_id"
          :to="`/stacks/${enc((state.jobs[selected] ?? jobs.find((j) => j.id === selected))!.stack_id)}`"
          class="text-xs font-normal text-emerald-700 hover:underline dark:text-emerald-400"
        >Open stack</RouterLink>
      </div>
      <JobOutput :id="selected" height="65vh" />
    </div>
  </div>
</template>
