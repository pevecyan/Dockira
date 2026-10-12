<script setup lang="ts">
import { computed } from 'vue'
import { badgeClass, isBusy, label, tone } from '../status'

const props = defineProps<{ status: string; exitCode?: number; text?: string; title?: string }>()
const t = computed(() => tone(props.status, props.exitCode))
</script>

<template>
  <span class="badge whitespace-nowrap" :class="badgeClass[t]" :title="title">
    <span class="h-1.5 w-1.5 shrink-0 rounded-full bg-current" :class="{ 'animate-pulse': isBusy(status) }" />
    {{ text ?? label(status) }}
  </span>
</template>
