<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import CopyField from '../components/CopyField.vue'
import Icon from '../components/Icon.vue'
import { ApiError, createToken, revokeToken, type TokenCreated } from '../api/client'
import { ago } from '../format'
import { guard } from '../session'
import { inboxes, tokens } from '../store'

const name = ref('')
const scope = ref<'all' | 'some'>('all')
const chosen = ref<string[]>([])
const filter = ref('')
const busy = ref(false)
const formError = ref('')
const created = ref<TokenCreated | null>(null)
const armed = ref<number | null>(null)
const revoking = ref<number | null>(null)
const rowError = ref('')
const showForm = ref(false)

const choices = computed(() => {
  const needle = filter.value.trim().toLowerCase()
  return (inboxes.data.value ?? []).filter((i) => !needle || i.slug.includes(needle))
})
const MAX_PICKS = 100 // the API accepts at most this many inboxes in one token
const canCreate = computed(() => name.value.trim() !== '' && (scope.value === 'all' || (chosen.value.length > 0 && chosen.value.length <= MAX_PICKS)))
const baseUrl = window.location.origin

async function create() {
  busy.value = true
  formError.value = ''
  try {
    const body = { name: name.value.trim(), ...(scope.value === 'all' ? { all_inboxes: true } : { inboxes: chosen.value }) }
    created.value = await guard((t) => createToken(t, body))
    name.value = ''
    chosen.value = []
    scope.value = 'all'
    showForm.value = false
    await tokens.refresh()
  } catch (e) {
    formError.value = e instanceof ApiError ? e.message : 'Could not create the token.'
  } finally {
    busy.value = false
  }
}

let disarm = 0
function revoke(id: number) {
  rowError.value = ''
  if (armed.value !== id) {
    armed.value = id
    window.clearTimeout(disarm)
    disarm = window.setTimeout(() => (armed.value = null), 4000)
    return
  }
  window.clearTimeout(disarm)
  armed.value = null
  void doRevoke(id)
}

async function doRevoke(id: number) {
  revoking.value = id
  try {
    await guard((t) => revokeToken(t, id))
    await tokens.refresh()
  } catch (e) {
    rowError.value = e instanceof ApiError ? e.message : 'Could not revoke the token.'
  } finally {
    revoking.value = null
  }
}

onMounted(() => {
  void tokens.refresh()
  if (!inboxes.data.value) void inboxes.refresh()
})
</script>

<template>
  <header class="top">
    <h1>Tokens</h1>
    <span class="grow"></span>
    <button type="button" class="btn primary" :disabled="created !== null" :title="created ? 'Store the new token first' : undefined" @click="showForm = !showForm"><Icon name="plus" />New token</button>
  </header>
  <p class="muted lead">Agent tokens let an agent read inboxes. They cannot use the admin API. Revoke one and it stops working at once.</p>

  <section v-if="created" class="card reveal" aria-live="polite">
    <span class="pill acc">Created: {{ created.record.name }}</span>
    <h2>Copy this token now</h2>
    <CopyField :value="created.token" label="The new token" />
    <p class="warn"><Icon name="alert" /> It is shown once. Only its hash is stored, so it cannot be shown again.</p>
    <div>
      <p class="label">Try it</p>
      <pre class="code">curl -s "{{ baseUrl }}/v1/inboxes/<span class="muted">SLUG</span>/messages?after=0" \
  -H "Authorization: Bearer {{ created.token }}"</pre>
    </div>
    <div><button type="button" class="btn" @click="created = null">I have stored it</button></div>
  </section>

  <form v-if="showForm" class="card form" @submit.prevent="create">
    <div class="field">
      <label for="tname">Name</label>
      <input id="tname" v-model="name" class="input" maxlength="100" placeholder="support agent" autocomplete="off" required />
    </div>
    <fieldset class="field">
      <legend class="label">Can read</legend>
      <label class="radio"><input v-model="scope" type="radio" value="all" /> Every inbox, including ones created later</label>
      <label class="radio"><input v-model="scope" type="radio" value="some" /> Only the inboxes I pick</label>
    </fieldset>
    <div v-if="scope === 'some'" class="field">
      <label for="tfilter">Inboxes</label>
      <input v-if="(inboxes.data.value?.length ?? 0) > 8" id="tfilter" v-model="filter" class="input" type="search" placeholder="Filter" autocomplete="off" />
      <div class="picks">
        <label v-for="i in choices" :key="i.id" class="radio"><input v-model="chosen" type="checkbox" :value="i.slug" /> <span class="mono">{{ i.slug }}</span></label>
        <p v-if="inboxes.data.value && !choices.length" class="muted">No inbox to pick.</p>
        <p v-if="chosen.length > MAX_PICKS" class="err">A token can name at most {{ MAX_PICKS }} inboxes; pick fewer, or choose every inbox.</p>
      </div>
    </div>
    <p v-if="formError" class="err" role="alert">{{ formError }}</p>
    <div class="actions">
      <button type="submit" class="btn primary" :disabled="busy || !canCreate">{{ busy ? 'Creating…' : 'Create token' }}</button>
      <button type="button" class="btn" @click="showForm = false">Cancel</button>
    </div>
  </form>

  <p class="sr-only" aria-live="polite">{{ armed !== null ? 'Press the highlighted Revoke button again to confirm.' : '' }}</p>
  <p v-if="tokens.error.value" class="err" role="alert">{{ tokens.error.value }}</p>
  <p v-if="rowError" class="err" role="alert">{{ rowError }}</p>

  <div v-if="tokens.data.value && tokens.data.value.length" class="tbl" role="table" aria-label="Tokens">
    <div class="row head" role="row"><span role="columnheader">Name</span><span role="columnheader">Can read</span><span role="columnheader">Created</span><span role="columnheader"><span class="sr-only">Actions</span></span></div>
    <div v-for="t in tokens.data.value" :key="t.id" class="row" role="row">
      <span role="cell" class="n">{{ t.name }}<small class="mono muted">#{{ t.id }}</small></span>
      <span role="cell" class="scope">
        <span v-if="t.all_inboxes" class="pill acc">every inbox</span>
        <span v-for="s in t.inboxes" v-else :key="s" class="pill mono">{{ s }}</span>
      </span>
      <span role="cell" class="muted">{{ ago(t.created_at) }}</span>
      <span role="cell" class="right">
        <button type="button" class="btn danger" :class="{ armed: armed === t.id }" :disabled="revoking === t.id" @click="revoke(t.id)">
          {{ revoking === t.id ? 'Revoking…' : armed === t.id ? 'Click to confirm' : 'Revoke' }}
        </button>
      </span>
    </div>
  </div>
  <section v-else-if="tokens.data.value" class="card empty">
    <h2>No tokens yet</h2>
    <p class="muted">Create one to let an agent read an inbox.</p>
  </section>
  <div v-else class="card skel"><div class="skeleton line"></div><div class="skeleton line"></div></div>
</template>

<style scoped>
.top { display: flex; align-items: center; gap: 12px; margin-bottom: 8px; }
h1 { font-size: 22px; }
.grow { flex: 1; }
.lead { margin-bottom: 20px; max-width: 62ch; }
.reveal { padding: 20px; display: grid; grid-template-columns: minmax(0, 1fr); gap: 14px; border-color: var(--accent); margin-bottom: 18px; justify-items: start; }
.reveal > * { max-width: 100%; }
.reveal h2 { font-size: 17px; }
.reveal :deep(.copy) { width: min(100%, 560px); }
.warn { color: var(--warn); display: flex; gap: 8px; align-items: center; font-size: 13px; }
.warn svg { width: 16px; height: 16px; flex: none; }
.code { margin: 0; background: var(--panel2); border: 1px solid var(--line); border-radius: 8px; padding: 10px 12px; font: 12.5px/1.6 var(--font-mono); overflow-x: auto; max-width: 100%; }
.form { padding: 20px; display: grid; gap: 16px; margin-bottom: 18px; max-width: 560px; }
fieldset { border: 0; margin: 0; padding: 0; display: grid; gap: 8px; }
.radio { display: flex; gap: 8px; align-items: center; cursor: pointer; font-size: 13.5px; color: var(--fg); }
.picks { display: grid; gap: 6px; max-height: 200px; overflow: auto; padding: 4px 2px; }
.actions { display: flex; gap: 8px; }
.tbl { background: var(--panel); border: 1px solid var(--line); border-radius: var(--radius); overflow: hidden; }
.row { display: grid; grid-template-columns: minmax(0, 1.2fr) minmax(0, 1.6fr) 110px 150px; gap: 14px; align-items: center; padding: 12px 16px; border-top: 1px solid var(--line); }
.row.head { border-top: 0; background: var(--panel2); color: var(--mute); font-size: 11.5px; text-transform: uppercase; letter-spacing: 0.06em; padding-block: 8px; }
.n { font-weight: 500; display: flex; flex-direction: column; min-width: 0; }
small { font-size: 12px; font-weight: 400; }
.scope { display: flex; gap: 6px; flex-wrap: wrap; }
.right { text-align: right; }
.empty { padding: 28px; display: grid; gap: 8px; }
.empty h2 { font-size: 17px; }
.skel { padding: 16px; display: grid; gap: 14px; }
.line { height: 2.2rem; }
@media (max-width: 760px) {
  .row { grid-template-columns: minmax(0, 1fr) auto; }
  .row.head { display: none; }
  .row > :nth-child(2) { grid-column: 1 / -1; order: 3; }
  .row > :nth-child(3) { order: 2; text-align: right; }
  .row > :nth-child(4) { grid-column: 1 / -1; order: 4; text-align: left; }
}
</style>
