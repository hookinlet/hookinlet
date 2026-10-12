<script setup lang="ts">
import { ref } from 'vue'
import { ApiError, getStatus, type Status } from './api/client'
import { bytes } from './format'

// The admin token is held here, in memory, and nowhere else.
const token = ref('')
const status = ref<Status | null>(null)
const error = ref('')
const busy = ref(false)

async function signIn() {
  busy.value = true
  error.value = ''
  try {
    status.value = await getStatus(token.value)
  } catch (e) {
    status.value = null
    error.value =
      e instanceof ApiError && e.status === 401
        ? 'That is not the admin token for this server.'
        : e instanceof Error
          ? e.message
          : 'Could not reach the server.'
  } finally {
    busy.value = false
  }
}

function signOut() {
  token.value = ''
  status.value = null
}
</script>

<template>
  <main class="shell">
    <h1>hookinlet</h1>
    <form v-if="!status" class="card" @submit.prevent="signIn">
      <label for="token">Admin token</label>
      <input id="token" v-model="token" type="password" autocomplete="off" spellcheck="false" required />
      <p v-if="error" class="err" role="alert">{{ error }}</p>
      <button type="submit" :disabled="busy || !token">Sign in</button>
    </form>
    <section v-else class="card" aria-live="polite">
      <dl>
        <dt>Version</dt>
        <dd>{{ status.version }}</dd>
        <dt>Schema</dt>
        <dd>{{ status.schema_version }}</dd>
        <dt>Database</dt>
        <dd>{{ bytes(status.database_bytes) }}</dd>
        <dt>Free on this volume</dt>
        <dd>{{ status.free_bytes === undefined ? 'unknown' : bytes(status.free_bytes) }}</dd>
      </dl>
      <button type="button" @click="signOut">Sign out</button>
    </section>
  </main>
</template>

<style scoped>
.shell { max-width: 28rem; margin: 12vh auto; padding: 0 1rem; display: grid; gap: 1.25rem; }
h1 { font-size: 1.5rem; letter-spacing: -0.02em; margin: 0; }
.card { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; padding: 1.25rem; display: grid; gap: 0.75rem; }
label { color: var(--mute); font-size: 0.85rem; }
input { font: inherit; background: var(--bg); color: var(--fg); border: 1px solid var(--line); border-radius: 8px; padding: 0.55rem 0.7rem; }
button { font: inherit; font-weight: 500; background: var(--accent); color: var(--accent-fg); border: 0; border-radius: 8px; padding: 0.55rem 0.9rem; cursor: pointer; }
button:disabled { opacity: 0.5; cursor: default; }
:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
.err { color: var(--bad); margin: 0; font-size: 0.9rem; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: 0.35rem 1rem; margin: 0; }
dt { color: var(--mute); }
dd { margin: 0; font-family: var(--font-mono); }
</style>
