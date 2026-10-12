<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ApiError, type Inbox } from '../api/client'
import { follow, getInbox, newest, type Filters, type Message } from '../api/messages'
import CopyField from '../components/CopyField.vue'
import MessageDetail from '../components/MessageDetail.vue'
import InboxSettings from '../components/InboxSettings.vue'
import MessageList from '../components/MessageList.vue'
import { compact } from '../format'
import { href, link, navigate } from '../router'
import { currentToken, guard, signOut } from '../session'

const props = defineProps<{ slug: string; seq: number | null; tab: 'messages' | 'settings' }>()

const info = ref<Inbox | null>(null)
const infoError = ref('')
const items = ref<Message[]>([])
const loading = ref(false)
const loadingMore = ref(false)
const hasMore = ref(false)
const listError = ref('')
const fresh = ref(new Set<number>())
const gapNotice = ref('')
const liveNote = ref('')
const latest = ref(0) // the newest seq the server has told us about

// What the person has typed or picked, and what the list on screen was loaded with. They differ
// for the moment between a change and the reload that follows it.
const filters = reactive<Filters>({ type: '', verified: '', since: '' })
const window_ = ref<'' | '1h' | '24h' | '7d'>('')
let active: Filters = { ...filters }
const MAX_KEPT = 1000

let generation = 0 // a load, a page of older messages, or a stream belongs to the filters it started with

function sinceOf(w: string): string {
  const hours = w === '1h' ? 1 : w === '24h' ? 24 : w === '7d' ? 168 : 0
  return hours ? new Date(Date.now() - hours * 3600_000).toISOString() : ''
}

async function loadInfo() {
  infoError.value = ''
  try {
    info.value = await guard((t) => getInbox(t, props.slug))
    latest.value = Math.max(latest.value, info.value.last_seq)
  } catch (e) {
    info.value = null
    infoError.value = e instanceof ApiError && e.status === 404 ? 'There is no inbox with that name.' : 'Could not load the inbox.'
  }
}

async function loadList() {
  const mine = ++generation
  stopLive()
  loading.value = true
  listError.value = ''
  active = { ...filters, since: sinceOf(window_.value) }
  try {
    const page = await guard((t) => newest(t, props.slug, active, 0))
    if (mine !== generation) return
    items.value = page.messages
    hasMore.value = page.messages.length > 0 && page.next !== null
    latest.value = Math.max(latest.value, page.latest_seq)
    gapNotice.value = ''
  } catch (e) {
    if (mine === generation) listError.value = e instanceof ApiError ? e.message : 'Could not load messages.'
  } finally {
    if (mine === generation) loading.value = false
  }
  if (mine === generation && wantLive.value) startLive()
}

async function loadOlder() {
  const oldest = items.value[items.value.length - 1]
  if (!oldest || loadingMore.value) return
  const mine = generation
  loadingMore.value = true
  try {
    const page = await guard((t) => newest(t, props.slug, active, oldest.seq))
    if (mine !== generation) return
    items.value = [...items.value, ...page.messages]
    hasMore.value = page.messages.length > 0 && page.next !== null
  } catch (e) {
    if (mine === generation) listError.value = e instanceof ApiError ? e.message : 'Could not load older messages.'
  } finally {
    loadingMore.value = false
  }
}

// ---- live tail ----
const wantLive = ref(false)
const liveState = ref<'off' | 'connecting' | 'live' | 'retrying' | 'failed'>('off')
let abort: AbortController | null = null

function startLive() {
  stopLive()
  abort = new AbortController()
  liveState.value = 'connecting'
  liveNote.value = ''
  // Follow from the newest seq known, from the list or from the server's own latest_seq, so an
  // inbox whose list is empty (a filter that matches nothing yet) does not start from zero.
  const after = Math.max(items.value[0]?.seq ?? 0, latest.value)
  void follow(
    currentToken(),
    props.slug,
    active,
    after,
    {
      message: (m) => {
        latest.value = Math.max(latest.value, m.seq)
        if (items.value.some((x) => x.seq === m.seq)) return
        if (items.value.length >= MAX_KEPT) hasMore.value = true // the tail that falls off can be loaded again
        items.value = [m, ...items.value].slice(0, MAX_KEPT)
        fresh.value = new Set([...fresh.value, m.seq])
        window.setTimeout(() => {
          const next = new Set(fresh.value)
          next.delete(m.seq)
          fresh.value = next
        }, 2000)
        if (info.value) info.value = { ...info.value, last_seq: Math.max(info.value.last_seq, m.seq), message_count: (info.value.message_count ?? 0) + 1 }
      },
      gap: (g) => {
        // Retention removed messages this page had not seen. The stream carries on from the
        // oldest that is left, so there is nothing to reload; say so, and refresh the counts.
        gapNotice.value = `Retention removed messages up to #${g.oldest_seq - 1} before they were shown.`
        void loadInfo()
      },
      state: (s) => (liveState.value = s),
      unauthorized: () => signOut(),
      failed: (reason) => {
        liveState.value = 'failed'
        liveNote.value = reason
        wantLive.value = false
      },
    },
    abort.signal,
  )
}

function stopLive() {
  abort?.abort()
  abort = null
  if (liveState.value !== 'failed') liveState.value = 'off'
}

function toggleLive() {
  wantLive.value = !wantLive.value
  liveNote.value = ''
  if (wantLive.value) startLive()
  else stopLive()
}

// ---- selection and keys ----
const detail = ref<InstanceType<typeof MessageDetail> | null>(null)

// select opens a message. A click moves focus to it (on a phone the list is hidden behind it);
// j and k keep the focus where it is, and keep the chosen row in view.
function select(seq: number, focus = true) {
  navigate(`/inboxes/${props.slug}/messages/${seq}`)
  void nextTick(() => {
    document.querySelector(`[data-seq="${seq}"]`)?.scrollIntoView({ block: 'nearest' })
    if (focus) detail.value?.focus()
  })
}
function close() {
  const was = props.seq
  navigate(`/inboxes/${props.slug}`)
  void nextTick(() => (document.querySelector(`[data-seq="${was}"] button`) as HTMLElement | null)?.focus())
}

function onKey(e: KeyboardEvent) {
  const el = e.target as HTMLElement | null
  if (e.metaKey || e.ctrlKey || e.altKey || (el && /^(INPUT|SELECT|TEXTAREA)$/.test(el.tagName))) return
  if (e.key !== 'j' && e.key !== 'k') return
  const list = items.value
  if (list.length === 0) return
  const at = props.seq === null ? -1 : list.findIndex((m) => m.seq === props.seq)
  const next = e.key === 'j' ? Math.min(list.length - 1, at + 1) : Math.max(0, at - 1) // j goes down the page, to older messages
  const target = list[next]
  if (target) select(target.seq, false)
}

const retention = computed(() => {
  const i = info.value
  if (!i) return ''
  return i.retention_days > 0 ? `${i.retention_days} days` : i.retention_max_count > 0 ? `newest ${compact(i.retention_max_count)}` : 'kept forever'
})
const hasFilters = computed(() => filters.type !== '' || filters.verified !== '' || window_.value !== '')
function clearFilters() {
  filters.type = ''
  filters.verified = ''
  window_.value = ''
}

// A change to a filter ends what the old filters were loading at once; the reload waits for the
// typing to pause.
let typed = 0
watch(
  () => [filters.type, filters.verified, window_.value],
  () => {
    generation++
    loadingMore.value = false
    window.clearTimeout(typed)
    typed = window.setTimeout(() => void loadList(), 300)
  },
)

// The messages are read when the Messages tab is showing, not behind the Settings tab.
watch(
  () => props.tab,
  (t) => {
    if (t === 'messages') void loadList()
    else stopLive()
  },
)

onMounted(() => {
  window.addEventListener('keydown', onKey)
  void loadInfo()
  if (props.tab === 'messages') void loadList()
})
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKey)
  window.clearTimeout(typed)
  generation++
  stopLive()
})
</script>

<template>
  <nav class="crumb" aria-label="Breadcrumb"><a :href="href('/inboxes')" @click="link($event, '/inboxes')">Inboxes</a> <span aria-hidden="true">/</span></nav>
  <header class="top">
    <h1>{{ info?.name || slug }}</h1>
    <span v-if="info" class="pill acc">{{ info.verifier }}</span>
    <span v-if="info?.disabled" class="pill warn">disabled</span>
    <span class="grow"></span>
    <button v-if="tab === 'messages'" type="button" class="btn" :class="{ live: wantLive }" :aria-pressed="wantLive" @click="toggleLive">
      <span class="dot" :class="liveState" aria-hidden="true"></span>
      {{ !wantLive ? 'Go live' : liveState === 'live' ? 'Live' : liveState === 'retrying' ? 'Reconnecting…' : 'Connecting…' }}
    </button>
  </header>
  <p v-if="infoError" class="err" role="alert">{{ infoError }}</p>
  <nav class="itabs" aria-label="Inbox">
    <a :href="href(`/inboxes/${slug}`)" :aria-current="tab === 'messages' ? 'page' : undefined" @click="link($event, `/inboxes/${slug}`)">Messages</a>
    <a :href="href(`/inboxes/${slug}/settings`)" :aria-current="tab === 'settings' ? 'page' : undefined" @click="link($event, `/inboxes/${slug}/settings`)">Settings</a>
  </nav>
  <p v-if="liveNote" class="err" role="alert">Live stopped: {{ liveNote }}</p>

  <section v-if="info && tab === 'messages'" class="facts card" aria-label="Inbox">
    <div><span class="k">Stored</span><span class="v num">{{ compact(info.message_count ?? 0) }}</span></div>
    <div><span class="k">Newest</span><span class="v num mono">#{{ info.last_seq }}</span></div>
    <div><span class="k">Keeps</span><span class="v">{{ retention }}</span></div>
    <div class="wide"><span class="k">Senders post to</span><CopyField :value="info.ingest_path" label="Ingest path" /></div>
  </section>

  <InboxSettings v-if="tab === 'settings' && info" :inbox="info" @updated="info = $event" @deleted="navigate('/inboxes')" />

  <template v-if="tab === 'messages'">
  <div class="bar">
    <div class="field"><label for="ftype">Event type</label><input id="ftype" v-model="filters.type" class="input" placeholder="invoice.paid" autocomplete="off" spellcheck="false" /></div>
    <div class="field"><label for="fver">Verification</label>
      <select id="fver" v-model="filters.verified" class="input"><option value="">Any</option><option value="true">Verified</option><option value="false">Not verified</option></select></div>
    <div class="field"><label for="fwin">Received</label>
      <select id="fwin" v-model="window_" class="input"><option value="">Any time</option><option value="1h">Last hour</option><option value="24h">Last 24 hours</option><option value="7d">Last 7 days</option></select></div>
    <button v-if="hasFilters" type="button" class="btn quiet" @click="clearFilters">Clear filters</button>
  </div>
  <p v-if="gapNotice" class="notice" role="status">{{ gapNotice }}</p>
  <p v-if="listError" class="err" role="alert">{{ listError }}</p>

  <div class="work" :class="{ picked: seq !== null }">
    <div class="listcol card">
      <div v-if="loading" class="pad"><div class="skeleton row"></div><div class="skeleton row"></div><div class="skeleton row"></div></div>
      <template v-else-if="items.length">
        <MessageList :messages="items" :selected="seq" :fresh="fresh" @select="select" />
        <div v-if="hasMore" class="more"><button type="button" class="btn" :disabled="loadingMore || loading" @click="loadOlder">{{ loadingMore ? 'Loading…' : 'Older messages' }}</button></div>
        <p class="hint muted">j and k move through the list.</p>
      </template>
      <div v-else class="pad empty">
        <h2>{{ hasFilters ? 'Nothing matches these filters' : 'No messages yet' }}</h2>
        <p class="muted">{{ hasFilters ? 'Change or clear a filter.' : 'Send the inbox a webhook, or go live to see the next one arrive.' }}</p>
      </div>
    </div>
    <MessageDetail v-if="seq !== null" ref="detail" :slug="slug" :seq="seq" @close="close" />
    <div v-else class="placeholder card"><p class="muted">Pick a message to read it.</p></div>
  </div>
  </template>
</template>

<style scoped>
.itabs { display: flex; gap: 20px; border-bottom: 1px solid var(--line); margin-bottom: 16px; }
.itabs a { padding: 8px 0; color: var(--mute); border-bottom: 2px solid transparent; font-weight: 500; }
.itabs a:hover { color: var(--fg); text-decoration: none; }
.itabs a[aria-current='page'] { color: var(--fg); border-bottom-color: var(--accent); }
.crumb { font-size: 13px; color: var(--mute); margin-bottom: 4px; display: flex; gap: 6px; }
.top { display: flex; align-items: center; gap: 12px; margin-bottom: 16px; flex-wrap: wrap; }
h1 { font-size: 22px; }
.grow { flex: 1; }
.dot { width: 8px; height: 8px; border-radius: 50%; background: var(--mute); display: inline-block; }
.btn.live { border-color: var(--accent); color: var(--accent); }
.dot.live { background: var(--ok); animation: pulse 2s infinite; }
.dot.connecting, .dot.retrying { background: var(--warn); }
@keyframes pulse { 50% { opacity: 0.35; } }
.facts { display: grid; grid-template-columns: repeat(3, auto) minmax(0, 1fr); gap: 8px 28px; padding: 12px 16px; margin-bottom: 14px; align-items: center; }
.facts > div { display: grid; gap: 2px; min-width: 0; }
.k { font-size: 12px; color: var(--mute); }
.v { font-weight: 600; font-size: 15px; }
.bar { display: flex; gap: 12px; align-items: end; flex-wrap: wrap; margin-bottom: 12px; }
.bar .field { min-width: 150px; }
.notice { background: var(--warn-soft); color: var(--warn); padding: 8px 12px; border-radius: 8px; margin-bottom: 12px; font-size: 13px; }
.work { display: grid; grid-template-columns: minmax(300px, 380px) minmax(0, 1fr); gap: 14px; align-items: start; }
.listcol { overflow: hidden; }
.pad { padding: 14px; display: grid; gap: 10px; }
.row { height: 3rem; }
.empty h2 { font-size: 16px; }
.more { padding: 10px 14px; display: flex; justify-content: center; }
.hint { font-size: 12px; padding: 0 14px 12px; }
.placeholder { padding: 40px 20px; text-align: center; }
@media (max-width: 900px) {
  .facts { grid-template-columns: repeat(2, auto); }
  .facts .wide { grid-column: 1 / -1; }
  .work { grid-template-columns: minmax(0, 1fr); }
  .work.picked .listcol { display: none; }
  .placeholder { display: none; }
}
</style>
