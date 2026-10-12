<script setup lang="ts">
import FeedPanel from '../components/FeedPanel.vue'
</script>

<template>
  <div class="space-y-3">
    <FeedPanel height="calc(100vh - 9rem)" />
    <p class="muted text-xs">The last 300 Docker events and job messages since Dockira started. Health checks and exec calls are left out.</p>
  </div>
</template>
