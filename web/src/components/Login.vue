<script setup lang="ts">
import { ref } from 'vue'
import { api } from '../api'
import { init, state } from '../state'

const password = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  busy.value = true
  error.value = ''
  try {
    await api.login(password.value)
    password.value = ''
    Object.assign(state, { loginRequired: false, error: '' })
    await init()
  } catch (e: any) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="flex min-h-screen items-center justify-center px-4">
    <form class="w-full max-w-sm space-y-4" @submit.prevent="submit">
      <div class="mb-8 text-center">
        <img src="/logo.png" alt="" class="mx-auto h-32 w-32" width="128" height="128" />
        <h1 class="mt-3 text-3xl font-semibold tracking-tight">Dockira</h1>
        <p class="mt-1 font-medium text-zinc-700 dark:text-zinc-300">Your stacks. Your server. Your control.</p>
      </div>
      <input
        v-model="password"
        type="password"
        class="input !px-4 !py-2.5"
        placeholder="Enter your password"
        aria-label="Password"
        autocomplete="current-password"
        autofocus
        required
      />
      <div v-if="error" class="rounded-lg border border-red-300 bg-red-50 px-3 py-2 text-sm text-red-800 dark:border-red-900 dark:bg-red-950/50 dark:text-red-200">
        {{ error }}
      </div>
      <button type="submit" class="btn btn-primary w-full !py-2.5" :disabled="busy || !password">{{ busy ? 'Signing in…' : 'Sign in' }}</button>
    </form>
  </div>
</template>
