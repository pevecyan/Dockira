<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, type LoginEvent, type ServerSettings } from '../api'
import { ago, dateTime, enc } from '../format'
import { signOut, state, toast } from '../state'
import { dark, toggleTheme } from '../theme'

const server = ref<ServerSettings | null>(null)
const logins = ref<LoginEvent[]>([])
onMounted(async () => {
  try {
    ;[server.value, logins.value] = await Promise.all([api.serverSettings(), api.logins().then((l) => l.events)])
  } catch (e: any) {
    toast('bad', e.message)
  }
})
const kindText: Record<string, string> = { login: 'login', login_failed: 'wrong password', confirm: 'password confirmed', logout: 'logout', lockout: 'lockout' }
</script>

<template>
  <div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
    <div class="space-y-4">
      <div class="card">
        <div class="card-h">This browser</div>
        <div class="space-y-3 px-4 py-4 text-sm">
          <div class="flex items-center justify-between">
            <span>Theme</span>
            <button class="btn btn-sm" @click="toggleTheme">{{ dark ? 'Switch to day mode' : 'Switch to night mode' }}</button>
          </div>
          <div class="flex items-center justify-between">
            <span>Session</span>
            <button class="btn btn-sm" @click="signOut">Log out</button>
          </div>
          <p class="muted text-xs">Logins last 30 days. Changing the password or secret_key in config.yaml logs out every browser.</p>
        </div>
      </div>

      <div class="card">
        <div class="card-h">Server configuration</div>
        <div v-if="server" class="px-4 py-3 text-sm">
          <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5">
            <dt class="text-zinc-500">version</dt><dd class="font-mono">{{ server.version }}</dd>
            <dt class="text-zinc-500">apps folder</dt><dd class="font-mono break-all">{{ server.apps_root }} <span class="muted">(depth {{ server.scan_depth }})</span></dd>
            <dt class="text-zinc-500">data</dt><dd class="font-mono break-all">{{ server.data_dir }}</dd>
            <dt class="text-zinc-500">listen</dt><dd class="font-mono">{{ server.listen }}</dd>
            <dt class="text-zinc-500">public URL</dt><dd class="font-mono break-all">{{ server.public_url || '– (links in notifications are left out)' }}</dd>
            <dt class="text-zinc-500">trusted proxies</dt><dd class="font-mono">{{ server.trusted_proxies.join(', ') || '–' }}</dd>
            <dt class="text-zinc-500">IP allowlist</dt><dd class="font-mono">{{ server.ip_allowlist.join(', ') || 'everyone' }}</dd>
            <dt class="text-zinc-500">read-only</dt><dd>{{ server.read_only ? 'yes' : 'no' }}</dd>
            <dt class="text-zinc-500">container shell</dt><dd>{{ server.exec_enabled ? 'enabled' : 'off' }}</dd>
            <dt class="text-zinc-500">own stack</dt>
            <dd><RouterLink v-if="server.self_stack" :to="`/stacks/${enc(server.self_stack)}`" class="text-emerald-700 hover:underline dark:text-emerald-400">{{ server.self_stack }}</RouterLink><template v-else>–</template></dd>
          </dl>
          <p class="muted mt-3 text-xs">
            These come from <span class="font-mono">config.yaml</span> next to Dockira's compose file and are read at start: edit the file over SSH and
            restart the stack. Update schedules and notifications are set in the UI.
          </p>
        </div>
      </div>

      <div class="card">
        <div class="card-h">Also set here</div>
        <ul class="divide-y divide-zinc-100 text-sm dark:divide-zinc-800/70">
          <li class="flex items-center justify-between px-4 py-2.5"><span>Update checks, maintenance window, defaults</span><RouterLink to="/updates" class="btn btn-sm">Updates</RouterLink></li>
          <li class="flex items-center justify-between px-4 py-2.5"><span>Notification channels and rules</span><RouterLink to="/notifications" class="btn btn-sm">Notifications</RouterLink></li>
          <li class="flex items-center justify-between px-4 py-2.5"><span>Who changed what</span><RouterLink to="/audit" class="btn btn-sm">Audit log</RouterLink></li>
        </ul>
      </div>
    </div>

    <div class="card self-start">
      <div class="card-h">Recent logins</div>
      <div v-if="!logins.length" class="muted px-4 py-6 text-center text-sm">None yet.</div>
      <ul class="max-h-[70vh] divide-y divide-zinc-100 overflow-auto text-sm dark:divide-zinc-800/70">
        <li v-for="(l, i) in logins" :key="i" class="flex flex-wrap items-center gap-x-3 gap-y-0.5 px-4 py-2">
          <span :class="l.ok ? 'text-emerald-600' : 'text-red-600'">{{ l.ok ? '✓' : '✕' }}</span>
          <span class="font-mono text-xs">{{ l.ip }}</span>
          <span>{{ kindText[l.kind] ?? l.kind }}</span>
          <span class="muted ml-auto text-xs" :title="dateTime(l.time)">{{ ago(l.time) }}</span>
          <span class="muted w-full truncate text-[11px]" :title="l.ua">{{ l.ua }}</span>
        </li>
      </ul>
    </div>
    <div v-if="state.info?.read_only" class="muted text-xs">Read-only mode is on.</div>
  </div>
</template>
