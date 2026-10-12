<script setup lang="ts">
import type { Message } from '../api/messages'
import { ago } from '../format'

defineProps<{ messages: Message[]; selected: number | null; fresh: Set<number> }>()
defineEmits<{ select: [seq: number] }>()

function status(m: Message): { cls: string; text: string } {
  if (m.verified) return { cls: 'ok', text: 'verified' }
  if (m.verify_error) return { cls: 'bad', text: 'failed' }
  return { cls: '', text: 'unverified' }
}
</script>

<template>
  <ul class="list" role="listbox" aria-label="Messages">
    <li v-for="m in messages" :key="m.seq" role="option" :aria-selected="selected === m.seq" class="msg" :class="{ on: selected === m.seq, fresh: fresh.has(m.seq) }">
      <button type="button" @click="$emit('select', m.seq)">
        <span class="l1">
          <span class="seq mono">#{{ m.seq }}</span>
          <span class="ev">{{ m.event_type ?? `${m.method} ${m.path || '/'}` }}</span>
          <time class="t" :datetime="m.received_at" :title="m.received_at">{{ ago(m.received_at) }}</time>
        </span>
        <span class="l2">
          <span class="pill" :class="status(m).cls">{{ status(m).text }}</span>
          <span class="mono muted">{{ m.size }} B</span>
          <span v-if="m.delivery_id" class="mono muted id">{{ m.delivery_id }}</span>
        </span>
      </button>
    </li>
  </ul>
</template>

<style scoped>
.list { list-style: none; margin: 0; padding: 0; }
.msg { border-bottom: 1px solid var(--line); }
.msg button { display: grid; gap: 4px; width: 100%; text-align: left; background: none; border: 0; padding: 10px 14px; cursor: pointer; }
.msg button:hover { background: var(--panel2); }
.msg.on button { background: var(--accent-soft); }
.msg.fresh button { animation: arrive 1.6s ease-out; }
@keyframes arrive { from { background: var(--accent-soft); } }
.l1 { display: flex; gap: 8px; align-items: baseline; min-width: 0; }
.seq { color: var(--mute); font-size: 12px; flex: none; }
.ev { font-weight: 500; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.t { margin-left: auto; color: var(--mute); font-size: 12px; white-space: nowrap; flex: none; }
.l2 { display: flex; gap: 8px; align-items: center; font-size: 12px; min-width: 0; }
.id { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
