<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api } from '../api'
import { state } from '../state'
import Modal from './Modal.vue'

// Yes/no dialog for destructive actions (state.dialog), and the password prompt the API asks
// for before dangerous actions (state.passwordPrompt).
const typed = ref('')
watch(
  () => state.dialog,
  () => (typed.value = ''),
)
const canConfirm = computed(() => !state.dialog?.typed || typed.value === state.dialog.typed)

function answer(ok: boolean) {
  const d = state.dialog
  state.dialog = null
  d?.resolve(ok && canConfirm.value)
}

const password = ref('')
const pwError = ref('')
const busy = ref(false)
async function confirmPassword() {
  busy.value = true
  pwError.value = ''
  try {
    await api.confirm(password.value)
    const r = state.passwordPrompt
    state.passwordPrompt = null
    password.value = ''
    r?.(true)
  } catch (e: any) {
    pwError.value = e.message
  } finally {
    busy.value = false
  }
}
function answerText(ok: boolean) {
  const p = state.textPrompt
  state.textPrompt = null
  p?.resolve(ok && p.value.trim() ? p.value.trim() : null)
}

function cancelPassword() {
  const r = state.passwordPrompt
  state.passwordPrompt = null
  password.value = ''
  pwError.value = ''
  r?.(false)
}
</script>

<template>
  <Modal v-if="state.dialog" :title="state.dialog.title" @close="answer(false)">
    <form class="space-y-3 px-4 py-4 text-sm" @submit.prevent="answer(true)">
      <p class="text-zinc-600 dark:text-zinc-300">{{ state.dialog.text }}</p>
      <div v-if="state.dialog.typed">
        <label class="label">Type <b class="font-mono">{{ state.dialog.typed }}</b> to confirm</label>
        <input v-model="typed" class="input font-mono" autocomplete="off" spellcheck="false" />
      </div>
      <div class="flex justify-end gap-2 pt-1">
        <button type="button" class="btn" @click="answer(false)">Cancel</button>
        <button type="submit" class="btn" :class="state.dialog.danger ? 'btn-danger' : 'btn-primary'" :disabled="!canConfirm">{{ state.dialog.confirm }}</button>
      </div>
    </form>
  </Modal>

  <Modal v-if="state.textPrompt" :title="state.textPrompt.title" @close="answerText(false)">
    <form class="space-y-3 px-4 py-4 text-sm" @submit.prevent="answerText(true)">
      <label class="label" for="text-prompt">{{ state.textPrompt.label }}</label>
      <input id="text-prompt" v-model="state.textPrompt.value" class="input font-mono" :placeholder="state.textPrompt.placeholder" autocomplete="off" spellcheck="false" />
      <div class="flex justify-end gap-2 pt-1">
        <button type="button" class="btn" @click="answerText(false)">Cancel</button>
        <button type="submit" class="btn btn-primary" :disabled="!state.textPrompt.value.trim()">OK</button>
      </div>
    </form>
  </Modal>

  <Modal v-if="state.passwordPrompt" title="Confirm your password" @close="cancelPassword">
    <form class="space-y-3 px-4 py-4 text-sm" @submit.prevent="confirmPassword">
      <p class="muted">This shows or changes secrets, or can't be undone, so it needs your password again (valid for 10 minutes).</p>
      <input v-model="password" type="password" class="input" placeholder="Password" autocomplete="current-password" aria-label="Password" />
      <div v-if="pwError" class="text-sm text-red-600 dark:text-red-400">{{ pwError }}</div>
      <div class="flex justify-end gap-2 pt-1">
        <button type="button" class="btn" @click="cancelPassword">Cancel</button>
        <button type="submit" class="btn btn-primary" :disabled="busy || !password">Confirm</button>
      </div>
    </form>
  </Modal>
</template>
