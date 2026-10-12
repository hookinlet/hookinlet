<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ApiError, deleteInbox, patchInbox, rotateIngestKey, setSecret, type Inbox, type PatchInbox } from '../api/client'
import { guard } from '../session'
import { inboxes } from '../store'
import { buildConfig, missing, presetOf, valuesOf } from '../verifiers'
import CopyField from './CopyField.vue'
import RetentionFields from './RetentionFields.vue'
import VerifierFields from './VerifierFields.vue'

const props = defineProps<{ inbox: Inbox }>()
const emit = defineEmits<{ updated: [Inbox]; deleted: [] }>()

const name = ref('')
const disabled = ref(false)
const pointer = ref('')
const maxBody = ref(0)
const days = ref(0)
const count = ref(0)
const onFail = ref<'reject' | 'store'>('reject')
const verifier = ref('none')
const values = ref<Record<string, string>>({})
const newSecret = ref('')

// Fill the form from the inbox, and again whenever the inbox changes under it (after a save).
function fill(i: Inbox) {
  name.value = i.name
  disabled.value = i.disabled
  pointer.value = i.event_type_pointer
  maxBody.value = i.max_body_bytes
  days.value = i.retention_days
  count.value = i.retention_max_count
  onFail.value = i.on_verify_fail as 'reject' | 'store'
  verifier.value = i.verifier
  values.value = valuesOf(i.verifier_config as Record<string, unknown>)
}
watch(() => props.inbox, fill, { immediate: true })

const preset = computed(() => presetOf(verifier.value))
const gaps = computed(() => (preset.value ? missing(preset.value, values.value) : []))
const verifierChanged = computed(() => verifier.value !== props.inbox.verifier)
const saving = ref(false)
const saved = ref('')
const error = ref('')

async function save() {
  if (!preset.value) return
  saving.value = true
  error.value = ''
  saved.value = ''
  const body: PatchInbox = {
    name: name.value.trim(),
    disabled: disabled.value,
    event_type_pointer: pointer.value.trim(),
    max_body_bytes: maxBody.value,
    retention_days: days.value,
    retention_max_count: count.value,
    on_verify_fail: onFail.value,
    verifier: verifier.value,
    verifier_config: buildConfig(preset.value, values.value) as unknown as PatchInbox['verifier_config'],
  }
  try {
    const updated = await guard((t) => patchInbox(t, props.inbox.slug, body))
    emit('updated', updated)
    saved.value = verifierChanged.value ? 'Saved. Changing the verifier removed its secret: set a new one below.' : 'Saved.'
    void inboxes.refresh()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Could not save.'
  } finally {
    saving.value = false
  }
}

// ---- the secret ----
const secretBusy = ref(false)
const secretMsg = ref('')
const secretErr = ref('')
async function saveSecret() {
  secretBusy.value = true
  secretErr.value = ''
  secretMsg.value = ''
  try {
    const updated = await guard((t) => setSecret(t, props.inbox.slug, newSecret.value))
    newSecret.value = ''
    emit('updated', updated)
    secretMsg.value = 'The secret was replaced.'
  } catch (e) {
    secretErr.value = e instanceof ApiError ? e.message : 'Could not set the secret.'
  } finally {
    secretBusy.value = false
  }
}

// ---- rotating the ingest key, deleting ----
const rotateArmed = ref(false)
const rotating = ref(false)
const rotated = ref<Inbox | null>(null)
const rotateErr = ref('')
async function rotate() {
  if (!rotateArmed.value) {
    rotateArmed.value = true
    window.setTimeout(() => (rotateArmed.value = false), 5000)
    return
  }
  rotateArmed.value = false
  rotating.value = true
  rotateErr.value = ''
  try {
    rotated.value = await guard((t) => rotateIngestKey(t, props.inbox.slug))
    emit('updated', rotated.value)
  } catch (e) {
    rotateErr.value = e instanceof ApiError ? e.message : 'Could not rotate the key.'
  } finally {
    rotating.value = false
  }
}

const confirmSlug = ref('')
const deleting = ref(false)
const deleteErr = ref('')
async function remove() {
  deleting.value = true
  deleteErr.value = ''
  try {
    await guard((t) => deleteInbox(t, props.inbox.slug))
    void inboxes.refresh()
    emit('deleted')
  } catch (e) {
    deleteErr.value = e instanceof ApiError ? e.message : 'Could not delete the inbox.'
    deleting.value = false
  }
}
</script>

<template>
  <div class="settings">
    <form class="card sect" @submit.prevent="save">
      <h2>General</h2>
      <div class="field">
        <label for="s-name">Name</label>
        <input id="s-name" v-model="name" class="input" maxlength="200" autocomplete="off" required />
      </div>
      <label class="radio"><input v-model="disabled" type="checkbox" /> Disabled: deliveries get a 404, and agents can still read what it holds</label>
      <div class="field">
        <label for="s-pointer">Event type pointer</label>
        <input id="s-pointer" v-model="pointer" class="input mono" autocomplete="off" spellcheck="false" placeholder="/type" />
      </div>
      <div class="field">
        <label for="s-max">Largest body (bytes)</label>
        <input id="s-max" v-model.number="maxBody" class="input" type="number" min="0" max="10485760" />
        <p class="muted help">0 means the default, 1 MiB.</p>
      </div>
      <RetentionFields v-model:days="days" v-model:count="count" id-prefix="s" />
      <fieldset class="field">
        <legend class="label">When a signature fails</legend>
        <label class="radio"><input v-model="onFail" type="radio" value="reject" /> Refuse the delivery</label>
        <label class="radio"><input v-model="onFail" type="radio" value="store" /> Refuse it, but keep a copy marked as failed</label>
      </fieldset>

      <h2>Verification</h2>
      <VerifierFields v-model:verifier="verifier" v-model:values="values" :with-secret="false" id-prefix="s" />
      <p v-if="verifierChanged" class="warn small">Changing the verifier removes the secret it had. Set a new one after saving.</p>

      <p v-if="error" class="err" role="alert">{{ error }}</p>
      <p v-if="saved" class="ok" role="status">{{ saved }}</p>
      <div><button type="submit" class="btn primary" :disabled="saving || !name.trim() || gaps.length > 0">{{ saving ? 'Saving…' : 'Save changes' }}</button></div>
    </form>

    <section v-if="preset && preset.secret" class="card sect">
      <h2>Secret</h2>
      <p class="muted">
        {{ inbox.has_verifier_secret ? 'A secret is set. It is stored sealed and cannot be shown. Enter a new one to replace it.' : 'No secret is set, so deliveries fail until you set one.' }}
      </p>
      <form class="inline" @submit.prevent="saveSecret">
        <div class="field grow">
          <label for="s-secret">{{ preset.secret.label }}</label>
          <input id="s-secret" v-model="newSecret" class="input mono" type="password" autocomplete="off" spellcheck="false" :placeholder="preset.secret.placeholder" />
        </div>
        <button type="submit" class="btn primary" :disabled="secretBusy || !newSecret">{{ secretBusy ? 'Saving…' : inbox.has_verifier_secret ? 'Replace secret' : 'Set secret' }}</button>
      </form>
      <p v-if="secretErr" class="err" role="alert">{{ secretErr }}</p>
      <p v-if="secretMsg" class="ok" role="status">{{ secretMsg }}</p>
    </section>

    <section class="card sect">
      <h2>Ingest URL</h2>
      <CopyField :value="inbox.ingest_url ?? inbox.ingest_path" label="Ingest URL" />
      <p class="muted help">Whoever has this URL can post to the inbox. If it leaked, rotate it: the old URL stops working at once, and senders need the new one.</p>
      <div v-if="rotated" class="result" role="status">
        <p class="ok">A new URL is in use. Give it to the sender:</p>
        <CopyField :value="rotated.ingest_url ?? rotated.ingest_path" label="New ingest URL" />
      </div>
      <p v-if="rotateErr" class="err" role="alert">{{ rotateErr }}</p>
      <div><button type="button" class="btn danger" :class="{ armed: rotateArmed }" :disabled="rotating" @click="rotate">{{ rotating ? 'Rotating…' : rotateArmed ? 'Click again to rotate' : 'Rotate the ingest URL' }}</button></div>
    </section>

    <section class="card sect danger">
      <h2>Delete this inbox</h2>
      <p class="muted">Deletes the inbox and all of its messages for good. Tokens that can read only this inbox stop working. Type <code>{{ inbox.slug }}</code> to confirm.</p>
      <form class="inline" @submit.prevent="remove">
        <div class="field grow">
          <label for="s-confirm">Inbox slug</label>
          <input id="s-confirm" v-model="confirmSlug" class="input mono" autocomplete="off" spellcheck="false" />
        </div>
        <button type="submit" class="btn danger" :disabled="deleting || confirmSlug !== inbox.slug">{{ deleting ? 'Deleting…' : 'Delete inbox' }}</button>
      </form>
      <p v-if="deleteErr" class="err" role="alert">{{ deleteErr }}</p>
    </section>
  </div>
</template>

<style scoped>
.settings { display: grid; gap: 16px; max-width: 640px; }
.sect { padding: 20px; display: grid; gap: 16px; }
.sect h2 { font-size: 16px; }
.danger { border-color: color-mix(in srgb, var(--bad) 40%, var(--line)); }
.help, .small { font-size: 12.5px; }
.warn { color: var(--warn); }
.ok { color: var(--ok); font-size: 13px; }
.radio { display: flex; gap: 8px; align-items: center; font-size: 13.5px; cursor: pointer; }
fieldset { border: 0; margin: 0; padding: 0; display: grid; gap: 8px; min-width: 0; }
.inline { display: flex; gap: 10px; align-items: end; flex-wrap: wrap; }
.grow { flex: 1; min-width: 200px; }
.result { display: grid; gap: 8px; }
</style>
