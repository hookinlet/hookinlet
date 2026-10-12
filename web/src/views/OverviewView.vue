<script setup lang="ts">
import { computed, onMounted } from 'vue'
import InboxTable from '../components/InboxTable.vue'
import { ago, compact } from '../format'
import { link, href } from '../router'
import { inboxes, status } from '../store'
import { activity, incomplete, loadActivity, loadingActivity } from './useActivity'

const list = computed(() => inboxes.data.value ?? [])
const received = computed(() => list.value.reduce((n, i) => n + i.last_seq, 0))
const disabled = computed(() => list.value.filter((i) => i.disabled).length)
const lastDay = computed(() => Object.values(activity.value).reduce((n, a) => n + a.series.reduce((x, y) => x + y, 0), 0))
const capped = computed(() => incomplete.value || Object.values(activity.value).some((a) => a.capped))
const retention = computed(() => {
  const s = status.data.value
  if (!s) return null
  if (s.last_retention_at) return { value: ago(s.last_retention_at), sub: `deleted ${compact(s.last_retention_deleted ?? 0)}` }
  return { value: s.retention_interval_seconds > 0 ? 'not yet' : 'off', sub: s.retention_interval_seconds > 0 ? 'scheduled' : 'no schedule' }
})

async function load() {
  await Promise.all([inboxes.refresh(), status.refresh()])
  if (inboxes.data.value) void loadActivity(inboxes.data.value)
}
onMounted(load)
</script>

<template>
  <header class="top">
    <h1>Overview</h1>
    <span class="grow"></span>
    <button type="button" class="btn" :disabled="inboxes.loading.value" @click="load">Refresh</button>
    <a class="btn primary" :href="href('/new')" @click="link($event, '/new')">New inbox</a>
  </header>

  <p v-if="inboxes.error.value" class="err" role="alert">{{ inboxes.error.value }} <button type="button" class="btn" @click="load">Try again</button></p>

  <section class="stats" aria-label="Summary">
    <div class="stat card"><span class="k">Inboxes</span>
      <span v-if="inboxes.data.value" class="v num">{{ list.length }}</span><span v-else class="v skeleton sk"></span>
      <span class="s">{{ disabled ? `${disabled} disabled` : 'all accepting' }}</span></div>
    <div class="stat card"><span class="k">Received, all time</span>
      <span v-if="inboxes.data.value" class="v num">{{ compact(received) }}</span><span v-else class="v skeleton sk"></span>
      <span class="s">newest seq per inbox, summed</span></div>
    <div class="stat card"><span class="k">Last 24 hours</span>
      <span v-if="inboxes.data.value" class="v num">{{ capped ? '≥ ' : '' }}{{ compact(lastDay) }}</span><span v-else class="v skeleton sk"></span>
      <span class="s">messages received</span></div>
    <div class="stat card"><span class="k">Last retention run</span>
      <span v-if="retention" class="v num">{{ retention.value }}</span><span v-else class="v skeleton sk"></span>
      <span class="s">{{ retention?.sub ?? '' }}</span></div>
  </section>

  <section v-if="inboxes.data.value && list.length === 0" class="card empty">
    <h2>No inboxes yet</h2>
    <p class="muted">An inbox is a named place webhooks arrive and are checked and kept.</p>
    <p><a class="btn primary" :href="href('/new')" @click="link($event, '/new')">Create your first inbox</a></p>
  </section>
  <InboxTable v-else-if="inboxes.data.value" :inboxes="list" :loading-lines="loadingActivity" />
  <div v-else class="card skel"><div class="skeleton line"></div><div class="skeleton line"></div><div class="skeleton line"></div></div>

  <p class="muted foot">Tokens are managed on the <a :href="href('/tokens')" @click="link($event, '/tokens')">Tokens page</a>.</p>
</template>

<style scoped>
.top { display: flex; align-items: center; gap: 12px; margin-bottom: 20px; }
h1 { font-size: 22px; }
.grow { flex: 1; }
.stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; margin-bottom: 18px; }
.stat { padding: 14px 16px; display: grid; gap: 2px; align-content: start; min-width: 0; }
.k { font-size: 12px; color: var(--mute); }
.v { font-size: 26px; font-weight: 600; letter-spacing: -0.02em; line-height: 1.25; }
.sk { width: 5rem; height: 1.9rem; display: block; }
.s { font-size: 12px; color: var(--mute); min-height: 1.5em; }
.empty { padding: 28px; display: grid; gap: 8px; }
.empty h2 { font-size: 17px; }
.skel { padding: 16px; display: grid; gap: 14px; }
.line { height: 2.2rem; }
.foot { margin-top: 16px; font-size: 13px; }
</style>
