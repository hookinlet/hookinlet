<script setup lang="ts">
import { computed } from 'vue'

// How long messages are kept: forever, for a number of days, or the newest N. (The server accepts
// both limits at once; the console offers the common choices, and shows what is set.)
const days = defineModel<number>('days', { required: true })
const count = defineModel<number>('count', { required: true })
const props = defineProps<{ idPrefix: string }>()

const mode = computed<'forever' | 'days' | 'count' | 'both'>(() =>
  days.value > 0 && count.value > 0 ? 'both' : days.value > 0 ? 'days' : count.value > 0 ? 'count' : 'forever',
)
function pick(m: 'forever' | 'days' | 'count') {
  if (m === 'forever') (days.value = 0), (count.value = 0)
  else if (m === 'days') (days.value = days.value > 0 ? days.value : 30), (count.value = 0)
  else (count.value = count.value > 0 ? count.value : 10000), (days.value = 0)
}
</script>

<template>
  <fieldset class="field">
    <legend class="label">Keep messages</legend>
    <div class="chips" role="radiogroup" aria-label="Retention">
      <label v-for="o in [['forever', 'Forever'], ['days', 'For a number of days'], ['count', 'The newest N']] as const" :key="o[0]" class="chip" :class="{ on: mode === o[0] || (mode === 'both' && o[0] !== 'forever') }">
        <input type="radio" :name="`${props.idPrefix}-keep`" class="sr-only" :checked="mode === o[0]" @change="pick(o[0])" />{{ o[1] }}
      </label>
    </div>
    <div v-if="mode !== 'forever'" class="nums">
      <div v-if="mode === 'days' || mode === 'both'" class="field">
        <label :for="`${idPrefix}-days`">Days</label>
        <input :id="`${idPrefix}-days`" v-model.number="days" class="input" type="number" min="1" max="36500" />
      </div>
      <div v-if="mode === 'count' || mode === 'both'" class="field">
        <label :for="`${idPrefix}-count`">Newest messages</label>
        <input :id="`${idPrefix}-count`" v-model.number="count" class="input" type="number" min="1" />
      </div>
    </div>
    <p class="muted help">Retention runs on a schedule, and from the Maintenance page. Older messages are deleted for good.</p>
  </fieldset>
</template>

<style scoped>
fieldset { border: 0; margin: 0; padding: 0; display: grid; gap: 8px; min-width: 0; }
.chips { display: flex; gap: 6px; flex-wrap: wrap; }
.chip { border: 1px solid var(--field); background: var(--panel); border-radius: 8px; padding: 6px 12px; font-size: 13px; color: var(--mute); cursor: pointer; }
.chip.on { border-color: var(--accent); color: var(--accent); background: var(--accent-soft); }
.chip:focus-within { outline: 2px solid var(--accent); outline-offset: 2px; }
.nums { display: flex; gap: 12px; flex-wrap: wrap; }
.nums .field { width: 160px; }
.help { font-size: 12.5px; }
</style>
