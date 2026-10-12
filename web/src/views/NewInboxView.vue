<script setup lang="ts">
import { computed, ref } from 'vue'
import { ApiError, createInbox, type CreateInbox, type InboxCreated } from '../api/client'
import CopyField from '../components/CopyField.vue'
import RetentionFields from '../components/RetentionFields.vue'
import VerifierFields from '../components/VerifierFields.vue'
import { href, link, navigate } from '../router'
import { guard } from '../session'
import { inboxes } from '../store'
import { buildConfig, missing, presetOf } from '../verifiers'

const slug = ref('')
const name = ref('')
const verifier = ref('stripe')
const values = ref<Record<string, string>>({})
const secret = ref('')
const days = ref(0)
const count = ref(0)
const onFail = ref<'reject' | 'store'>('reject')
const pointer = ref('')
const maxBody = ref(0)
const busy = ref(false)
const error = ref('')
const created = ref<InboxCreated | null>(null)

const preset = computed(() => presetOf(verifier.value))
const slugOk = computed(() => /^[a-z0-9-]{3,64}$/.test(slug.value))
const gaps = computed(() => (preset.value ? missing(preset.value, values.value) : []))
const needsSecret = computed(() => !!preset.value?.secret && secret.value.trim() === '')
const canCreate = computed(() => slugOk.value && gaps.value.length === 0 && !busy.value)

async function submit() {
  if (!preset.value) return
  busy.value = true
  error.value = ''
  const body: CreateInbox = {
    slug: slug.value,
    verifier: verifier.value,
    on_verify_fail: onFail.value,
    retention_days: days.value,
    retention_max_count: count.value,
  }
  if (name.value.trim()) body.name = name.value.trim()
  const config = buildConfig(preset.value, values.value)
  if (Object.keys(config).length) body.verifier_config = config as unknown as CreateInbox['verifier_config'] // the generated type is stricter than the document
  if (preset.value.secret && secret.value.trim()) body.verifier_secret = secret.value
  if (pointer.value.trim()) body.event_type_pointer = pointer.value.trim()
  if (maxBody.value > 0) body.max_body_bytes = maxBody.value
  try {
    created.value = await guard((t) => createInbox(t, body))
    secret.value = ''
    void inboxes.refresh()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Could not create the inbox.'
  } finally {
    busy.value = false
  }
}

function another() {
  created.value = null
  slug.value = name.value = secret.value = ''
  values.value = {}
}

const curl = computed(() =>
  created.value
    ? `curl -s "${window.location.origin}/v1/inboxes/${created.value.inbox.slug}/messages?after=0" \\\n  -H "Authorization: Bearer ${created.value.agent_token}"`
    : '',
)
</script>

<template>
  <nav class="crumb" aria-label="Breadcrumb"><a :href="href('/inboxes')" @click="link($event, '/inboxes')">Inboxes</a> <span aria-hidden="true">/</span></nav>
  <h1>New inbox</h1>

  <section v-if="created" class="card reveal" aria-live="polite">
    <span class="pill acc">Created: {{ created.inbox.slug }}</span>
    <h2>Give the sender this URL</h2>
    <CopyField :value="created.inbox.ingest_url ?? created.inbox.ingest_path" label="Ingest URL" />
    <p class="muted help">Whoever has this URL can post to the inbox, so it is not guessable. <span v-if="created.inbox.verifier !== 'none'">Give the sender the same secret you entered.</span></p>
    <h2>Give your agent this token</h2>
    <CopyField :value="created.agent_token" label="Agent token" />
    <p class="warn">It is shown once. Only its hash is stored, so it cannot be shown again.</p>
    <ul v-if="created.warnings?.length" class="warnings"><li v-for="w in created.warnings" :key="w">{{ w }}</li></ul>
    <div><p class="label">Read it back</p><pre class="code">{{ curl }}</pre></div>
    <div class="actions">
      <a class="btn primary" :href="href(`/inboxes/${created.inbox.slug}`)" @click="link($event, `/inboxes/${created.inbox.slug}`)">Open the inbox</a>
      <button type="button" class="btn" @click="another">Create another</button>
    </div>
  </section>

  <form v-else class="card form" @submit.prevent="submit">
    <div class="field">
      <label for="slug">Slug</label>
      <input id="slug" v-model="slug" class="input mono" autocomplete="off" spellcheck="false" placeholder="stripe-prod" maxlength="64" required />
      <p class="help" :class="slug && !slugOk ? 'err' : 'muted'">3 to 64 characters: lowercase letters, digits and hyphens. It never changes.</p>
    </div>
    <div class="field">
      <label for="name">Name <span class="muted">(optional)</span></label>
      <input id="name" v-model="name" class="input" autocomplete="off" maxlength="200" placeholder="Stripe (production)" />
    </div>

    <VerifierFields v-model:verifier="verifier" v-model:values="values" v-model:secret="secret" with-secret id-prefix="new" />
    <p v-if="needsSecret" class="warn small">No secret yet: deliveries to this inbox will fail until you set one on its settings page.</p>

    <RetentionFields v-model:days="days" v-model:count="count" id-prefix="new" />

    <fieldset class="field">
      <legend class="label">When a signature fails</legend>
      <label class="radio"><input v-model="onFail" type="radio" value="reject" /> Refuse the delivery (the sender sees an error)</label>
      <label class="radio"><input v-model="onFail" type="radio" value="store" /> Refuse it, but keep a copy marked as failed</label>
    </fieldset>

    <details class="more">
      <summary>More settings</summary>
      <div class="field">
        <label for="pointer">Event type pointer</label>
        <input id="pointer" v-model="pointer" class="input mono" autocomplete="off" spellcheck="false" placeholder="/type" />
        <p class="muted help">A JSON pointer to the event type in the body, such as /type. For senders whose verifier does not say.</p>
      </div>
      <div class="field">
        <label for="maxbody">Largest body (bytes)</label>
        <input id="maxbody" v-model.number="maxBody" class="input" type="number" min="0" max="10485760" />
        <p class="muted help">0 means the default, 1 MiB. At most 10 MiB.</p>
      </div>
    </details>

    <p v-if="gaps.length" class="muted small">Still needed: {{ gaps.join(', ') }}.</p>
    <p v-if="error" class="err" role="alert">{{ error }}</p>
    <div class="actions">
      <button type="submit" class="btn primary" :disabled="!canCreate">{{ busy ? 'Creating…' : 'Create inbox' }}</button>
      <button type="button" class="btn" @click="navigate('/inboxes')">Cancel</button>
    </div>
  </form>
</template>

<style scoped>
.crumb { font-size: 13px; color: var(--mute); margin-bottom: 4px; display: flex; gap: 6px; }
h1 { font-size: 22px; margin-bottom: 18px; }
.form { padding: 22px; display: grid; gap: 20px; max-width: 620px; }
.help { font-size: 12.5px; }
.small { font-size: 12.5px; }
fieldset { border: 0; margin: 0; padding: 0; display: grid; gap: 8px; min-width: 0; }
.radio { display: flex; gap: 8px; align-items: center; font-size: 13.5px; cursor: pointer; }
.more { border: 1px solid var(--line); border-radius: 8px; padding: 10px 14px; }
.more summary { cursor: pointer; color: var(--mute); font-size: 13px; }
.more[open] summary { margin-bottom: 14px; }
.more > .field { margin-bottom: 14px; }
.actions { display: flex; gap: 8px; flex-wrap: wrap; }
.warn { color: var(--warn); font-size: 13px; }
.reveal { padding: 22px; display: grid; grid-template-columns: minmax(0, 1fr); gap: 12px; border-color: var(--accent); max-width: 680px; justify-items: start; }
.reveal > * { max-width: 100%; }
.reveal h2 { font-size: 16px; margin-top: 4px; }
.reveal :deep(.copy) { width: min(100%, 600px); }
.warnings { margin: 0; padding-left: 18px; color: var(--warn); font-size: 13px; }
.code { margin: 0; background: var(--panel2); border: 1px solid var(--line); border-radius: 8px; padding: 10px 12px; font: 12.5px/1.6 var(--font-mono); overflow-x: auto; max-width: 100%; }
a.btn:hover { text-decoration: none; }
</style>
