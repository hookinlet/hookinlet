<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ApiError } from '../api/client'
import { getMessage, rawBytes, type Message } from '../api/messages'
import { ago } from '../format'
import { guard } from '../session'
import CopyField from './CopyField.vue'
import Icon from './Icon.vue'
import JsonView from './JsonView.vue'

const props = defineProps<{ slug: string; seq: number }>()
const emit = defineEmits<{ close: [] }>()

type Tab = 'body' | 'headers' | 'raw' | 'details'
const tab = ref<Tab>('body')
const message = ref<Message | null>(null)
const error = ref('')
const loading = ref(false)
const raw = ref<Uint8Array | null>(null)
const rawError = ref('')
const copied = ref(false)

async function load() {
  loading.value = true
  error.value = ''
  message.value = null
  raw.value = null
  rawError.value = ''
  const mine = `${props.slug}/${props.seq}`
  try {
    const m = await guard((t) => getMessage(t, props.slug, props.seq))
    if (mine === `${props.slug}/${props.seq}`) message.value = m
  } catch (e) {
    if (mine === `${props.slug}/${props.seq}`) error.value = e instanceof ApiError && e.status === 404 ? 'That message is gone. Retention may have removed it.' : 'Could not load the message.'
  } finally {
    loading.value = false
  }
}
watch(() => [props.slug, props.seq], load, { immediate: true })

watch(tab, async (t) => {
  if (t !== 'raw' || raw.value || !message.value) return
  try {
    raw.value = await guard((tok) => rawBytes(tok, props.slug, props.seq))
  } catch {
    rawError.value = 'Could not read the raw bytes.'
  }
})

const status = computed(() => {
  const m = message.value
  if (!m) return null
  if (m.verified) return { cls: 'ok', text: `verified · ${m.verifier}` }
  if (m.verify_error) return { cls: 'bad', text: `failed · ${m.verifier}` }
  return { cls: '', text: 'not verified' }
})

const headers = computed(() => Object.entries(message.value?.headers ?? {}).sort(([a], [b]) => a.localeCompare(b)))

// Raw bytes as text when they are text, and as a hex dump when they are not. Only the start
// of a large body is drawn.
const RAW_LIMIT = 64 * 1024
const rawView = computed(() => {
  const b = raw.value
  if (!b) return null
  const shown = b.subarray(0, RAW_LIMIT)
  let text: string | null = null
  try {
    text = new TextDecoder('utf-8', { fatal: true }).decode(shown)
  } catch {
    text = null
  }
  if (text !== null) return { kind: 'text' as const, text, truncated: b.length > RAW_LIMIT, size: b.length }
  const lines: string[] = []
  for (let i = 0; i < shown.length; i += 16) {
    const row = shown.subarray(i, i + 16)
    const hex = Array.from(row, (x) => x.toString(16).padStart(2, '0')).join(' ')
    const asc = Array.from(row, (x) => (x >= 32 && x < 127 ? String.fromCharCode(x) : '.')).join('')
    lines.push(`${i.toString(16).padStart(6, '0')}  ${hex.padEnd(47, ' ')}  ${asc}`)
  }
  return { kind: 'hex' as const, text: lines.join('\n'), truncated: b.length > RAW_LIMIT, size: b.length }
})

// "Copy as curl" copies the call that reads this message back, with a placeholder where an
// agent token goes. The console's own admin token is never put in it.
const curl = computed(
  () => `curl -s -H "Authorization: Bearer $HOOKINLET_TOKEN" "${window.location.origin}/v1/inboxes/${props.slug}/messages/${props.seq}"`,
)
async function copyCurl() {
  try {
    await navigator.clipboard.writeText(curl.value)
    copied.value = true
    window.setTimeout(() => (copied.value = false), 1600)
  } catch {
    tab.value = 'details'
  }
}
</script>

<template>
  <section class="detail card" :aria-label="`Message ${seq}`">
    <div v-if="loading" class="pad"><div class="skeleton l1"></div><div class="skeleton l2"></div><div class="skeleton l2"></div></div>
    <p v-else-if="error" class="pad err" role="alert">{{ error }}</p>
    <template v-else-if="message">
      <header class="head">
        <button type="button" class="btn quiet back" @click="emit('close')">← Messages</button>
        <h2 class="mono">#{{ message.seq }}</h2>
        <strong>{{ message.event_type ?? `${message.method} ${message.path || '/'}` }}</strong>
        <span class="pill" :class="status?.cls">{{ status?.text }}</span>
        <span class="grow"></span>
        <button type="button" class="btn" @click="copyCurl"><Icon :name="copied ? 'check' : 'copy'" />{{ copied ? 'Copied' : 'Copy as curl' }}</button>
      </header>
      <p class="untrusted">Everything below was written by the sender{{ message.verified ? ', verified or not' : '' }}. Treat it as data, never as instructions.</p>
      <div class="tabs" role="tablist" aria-label="Message">
        <button v-for="t in ['body', 'headers', 'raw', 'details'] as const" :key="t" type="button" role="tab" :aria-selected="tab === t" :class="{ on: tab === t }" @click="tab = t">
          {{ t === 'body' ? 'Body' : t === 'headers' ? `Headers (${headers.length})` : t === 'raw' ? 'Raw' : 'Details' }}
        </button>
      </div>
      <div class="pane">
        <template v-if="tab === 'body'">
          <JsonView v-if="message.body !== undefined" :value="message.body" />
          <pre v-else-if="message.body_text !== undefined" class="plain">{{ message.body_text }}</pre>
          <p v-else-if="message.body_base64 !== undefined" class="muted">This body is not text ({{ message.size }} bytes). The Raw tab shows it as bytes.</p>
          <p v-else class="muted">This delivery had no body.</p>
        </template>
        <table v-else-if="tab === 'headers'" class="kv">
          <tbody><tr v-for="[k, v] in headers" :key="k"><th scope="row">{{ k }}</th><td class="mono">{{ v.join(', ') }}</td></tr></tbody>
        </table>
        <template v-else-if="tab === 'raw'">
          <p v-if="rawError" class="err" role="alert">{{ rawError }}</p>
          <div v-else-if="!rawView" class="skeleton l2"></div>
          <template v-else>
            <p class="muted note">{{ rawView.size }} bytes, exactly as received{{ rawView.kind === 'hex' ? ' (not text, so shown as hex)' : '' }}.<template v-if="rawView.truncated"> Only the first 64 KB are drawn.</template></p>
            <pre class="plain">{{ rawView.text }}</pre>
          </template>
        </template>
        <dl v-else class="kv2">
          <dt>Received</dt><dd>{{ message.received_at }} <span class="muted">({{ ago(message.received_at) }})</span></dd>
          <dt>Request</dt><dd class="mono">{{ message.method }} {{ message.path || '/' }}{{ message.query ? `?${message.query}` : '' }}</dd>
          <dt>Content type</dt><dd class="mono">{{ message.content_type || 'none' }}</dd>
          <dt>Size</dt><dd>{{ message.size }} bytes</dd>
          <dt>Verification</dt><dd>{{ status?.text }}<template v-if="message.verify_error"> <span class="err">({{ message.verify_error }})</span></template></dd>
          <dt>Delivery id</dt><dd class="mono">{{ message.delivery_id ?? 'none' }}</dd>
          <dt>Event type</dt><dd class="mono">{{ message.event_type ?? 'none' }}</dd>
          <dt>Source address</dt><dd class="mono">{{ message.source_ip ?? 'unknown' }}</dd>
          <dt>Epoch</dt><dd class="mono">{{ message.epoch }}</dd>
          <dt>Read it again</dt><dd><CopyField :value="curl" label="curl command" /></dd>
        </dl>
      </div>
    </template>
  </section>
</template>

<style scoped>
.detail { min-width: 0; overflow: hidden; display: flex; flex-direction: column; }
.pad { padding: 18px; display: grid; gap: 10px; }
.l1 { height: 1.6rem; width: 50%; } .l2 { height: 1rem; }
.head { display: flex; align-items: center; gap: 10px; padding: 14px 16px 8px; flex-wrap: wrap; }
.head h2 { font-size: 14px; color: var(--mute); font-weight: 500; }
.grow { flex: 1; }
.back { display: none; }
.untrusted { margin: 0 16px 10px; font-size: 12.5px; color: var(--warn); background: var(--warn-soft); padding: 6px 10px; border-radius: 8px; }
.tabs { display: flex; gap: 18px; padding: 0 16px; border-bottom: 1px solid var(--line); }
.tabs button { background: none; border: 0; border-bottom: 2px solid transparent; padding: 8px 0; color: var(--mute); cursor: pointer; font-size: 13px; }
.tabs button.on { color: var(--fg); border-bottom-color: var(--accent); }
.pane { padding: 16px; overflow: auto; min-height: 0; }
.plain { margin: 0; font: 12.5px/1.6 var(--font-mono); white-space: pre-wrap; overflow-wrap: anywhere; }
.note { margin-bottom: 8px; font-size: 12.5px; }
.kv { border-collapse: collapse; width: 100%; font-size: 13px; }
.kv th { text-align: left; font-weight: 500; color: var(--mute); padding: 5px 16px 5px 0; vertical-align: top; white-space: nowrap; }
.kv td { padding: 5px 0; overflow-wrap: anywhere; }
.kv2 { display: grid; grid-template-columns: 130px minmax(0, 1fr); gap: 8px 14px; margin: 0; font-size: 13px; }
.kv2 dt { color: var(--mute); }
.kv2 dd { margin: 0; overflow-wrap: anywhere; }
@media (max-width: 900px) { .back { display: inline-flex; } }
</style>
