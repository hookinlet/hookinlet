<script setup lang="ts">
import { ref } from 'vue'
import { ApiError } from '../api/client'
import { signIn } from '../session'

const token = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  busy.value = true
  error.value = ''
  try {
    await signIn(token.value.trim())
  } catch (e) {
    error.value =
      e instanceof ApiError && e.status === 401
        ? 'That is not this server’s admin token.'
        : e instanceof ApiError && e.status === 404
          ? 'This server has its admin API turned off.'
          : 'Could not reach the server. Check that it is running.'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="gate">
    <form class="card box" @submit.prevent="submit">
      <div class="logo"><i aria-hidden="true"></i>hookinlet</div>
      <div>
        <h1>Sign in</h1>
        <p class="muted">Use the admin token this server was started with.</p>
      </div>
      <div class="field">
        <label for="token">Admin token</label>
        <input id="token" v-model="token" class="input mono" type="password" autocomplete="off" spellcheck="false" autofocus required />
      </div>
      <p v-if="error" class="err" role="alert">{{ error }}</p>
      <button type="submit" class="btn primary center" :disabled="busy || !token.trim()">{{ busy ? 'Checking…' : 'Sign in' }}</button>
      <p class="muted small">The token stays in this tab’s memory. Closing or reloading the page signs you out.</p>
    </form>
  </main>
</template>

<style scoped>
.gate { min-height: 100%; display: grid; place-items: center; padding: 24px 16px; }
.box { width: min(100%, 380px); padding: 28px; display: grid; gap: 18px; box-shadow: var(--shadow); }
h1 { font-size: 22px; }
.center { justify-content: center; padding-block: 9px; }
.small { font-size: 12.5px; }
.logo { display: flex; align-items: center; gap: 9px; font-weight: 600; font-size: 17px; letter-spacing: -0.02em; }
.logo i { width: 22px; height: 22px; border: 2px solid var(--fg); border-top-color: transparent; border-radius: 0 0 6px 6px; position: relative; display: inline-block; }
.logo i::after { content: ''; position: absolute; left: 4px; top: 3px; width: 8px; height: 4px; border-radius: 2px; background: var(--accent); }
</style>
