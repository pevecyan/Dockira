<script setup lang="ts">
import { computed } from 'vue'
import { state } from '../state'
import JobOutput from './JobOutput.vue'
import Modal from './Modal.vue'

const job = computed(() => (state.jobPanel ? state.jobs[state.jobPanel] : undefined))
</script>

<template>
  <Modal v-if="state.jobPanel" :title="job?.title ?? `Job #${state.jobPanel}`" full @close="state.jobPanel = null">
    <template #actions>
      <RouterLink :to="`/jobs?id=${state.jobPanel}`" class="text-xs text-emerald-700 hover:underline dark:text-emerald-400" @click="state.jobPanel = null">All jobs</RouterLink>
    </template>
    <JobOutput :id="state.jobPanel" height="60vh" />
  </Modal>
</template>
