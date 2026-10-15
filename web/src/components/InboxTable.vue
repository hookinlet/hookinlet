<script setup lang="ts">
import type { Inbox } from '../api/client'
import { compact } from '../format'
import { href, link } from '../router'
import { activity } from '../views/useActivity'
import Sparkline from './Sparkline.vue'

defineProps<{ inboxes: Inbox[]; loadingLines?: boolean }>()
</script>

<template>
  <div class="tbl" role="table" aria-label="Inboxes">
    <div class="row head" role="row">
      <span role="columnheader">Inbox</span><span role="columnheader">Verifier</span><span role="columnheader" class="right">Received</span>
      <span role="columnheader">Last 24 h</span><span role="columnheader" class="right">Retention</span>
    </div>
    <div v-for="i in inboxes" :key="i.id" class="row" role="row">
      <span class="slug" role="cell">
        <span class="name"><a :href="href(`/inboxes/${i.slug}`)" class="slugline" @click="link($event, `/inboxes/${i.slug}`)">{{ i.slug }}</a><span v-if="i.disabled" class="pill">disabled</span></span>
        <small class="mono muted">{{ i.ingest_path }}</small>
      </span>
      <span role="cell" class="pills">
        <span v-if="i.verifier === 'none'" class="pill warn" title="Anyone with the ingest URL can post anything; every message is stored as not verified">none (unverified)</span>
        <span v-else class="pill acc">{{ i.verifier }}</span>
        <span v-if="!i.has_verifier_secret && i.verifier !== 'none'" class="pill warn" title="Deliveries fail until a secret is set">no secret</span>
      </span>
      <span role="cell" class="right num">{{ compact(i.last_seq) }}</span>
      <span role="cell" class="spark">
        <Sparkline v-if="activity[i.slug]" :values="activity[i.slug]!.series" />
        <span v-else class="muted" :title="loadingLines ? 'Loading' : 'Activity was not loaded for this inbox'">{{ loadingLines ? '…' : '–' }}</span>
        <small v-if="activity[i.slug]?.capped" class="muted" title="More than 200 in the last day; the line shows the newest 200">200+</small>
      </span>
      <span role="cell" class="right muted">
        {{ i.retention_days > 0 ? `${i.retention_days} d` : i.retention_max_count > 0 ? `newest ${compact(i.retention_max_count)}` : 'keep all' }}
      </span>
    </div>
  </div>
</template>

<style scoped>
.tbl { background: var(--panel); border: 1px solid var(--line); border-radius: var(--radius); overflow: hidden; }
.row { display: grid; grid-template-columns: minmax(0, 1.6fr) minmax(0, 1fr) 90px 150px 110px; gap: 14px; align-items: center; padding: 12px 16px; border-top: 1px solid var(--line); }
.row.head { border-top: 0; background: var(--panel2); color: var(--mute); font-size: 11.5px; text-transform: uppercase; letter-spacing: 0.06em; padding-block: 8px; }
.slug { display: flex; flex-direction: column; min-width: 0; }
.name { font-weight: 500; display: flex; align-items: center; gap: 8px; }
.slugline { color: var(--fg); }
.slugline:hover { color: var(--accent); }
small { font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.right { text-align: right; }
.pills { display: flex; gap: 6px; flex-wrap: wrap; }
.spark { display: flex; align-items: center; gap: 6px; }
@media (max-width: 760px) {
  .row { grid-template-columns: minmax(0, 1fr) auto; }
  .row.head { display: none; }
  .row > :nth-child(2) { order: 3; grid-column: 1; }
  .row > :nth-child(3) { text-align: right; }
  .row > :nth-child(4) { grid-column: 1; }
  .row > :nth-child(5) { grid-column: 2; }
}
</style>
