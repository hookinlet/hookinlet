<script setup lang="ts">
import { ref } from 'vue'

// How long messages are kept: forever, for a number of days, the newest N, or both limits (the
// server applies whichever is stricter). The chosen way is its own state, not worked out from the
// numbers, so emptying a box while typing a new value does not make the box disappear.
const days = defineModel<number>('days', { required: true })
const count = defineModel<number>('count', { required: true })
const props = defineProps<{ idPrefix: string }>()

type Mode = 'forever' | 'days' | 'count' | 'both'
const mode = ref<Mode>(days.value > 0 && count.value > 0 ? 'both' : days.value > 0 ? 'days' : count.value > 0 ? 'count' : 'forever')
const daysText = ref(days.value > 0 ? String(days.value) : '')
const countText = ref(count.value > 0 ? String(count.value) : '')

const OPTIONS: [Mode, string][] = [
  ['forever', 'Forever'],
  ['days', 'For a number of days'],
  ['count', 'The newest N'],
  ['both', 'Both limits'],
]

// A number the person typed, or 0 for an empty box (which the server reads as no limit).
const toNumber = (t: string) => {
  const n = Math.floor(Number(t))
  return Number.isFinite(n) && n > 0 ? n : 0
}
function apply() {
  days.value = mode.value === 'days' || mode.value === 'both' ? toNumber(daysText.value) : 0
  count.value = mode.value === 'count' || mode.value === 'both' ? toNumber(countText.value) : 0
}
function pick(m: Mode) {
  mode.value = m
  if ((m === 'days' || m === 'both') && daysText.value === '') daysText.value = '30'
  if ((m === 'count' || m === 'both') && countText.value === '') countText.value = '10000'
  apply()
}
</script>

<template>
  <fieldset class="field">
    <legend class="label">Keep messages</legend>
    <div class="chips" role="radiogroup" aria-label="Retention">
      <label v-for="o in OPTIONS" :key="o[0]" class="chip" :class="{ on: mode === o[0] }">
        <input type="radio" :name="`${props.idPrefix}-keep`" class="sr-only" :checked="mode === o[0]" @change="pick(o[0])" />{{ o[1] }}
      </label>
    </div>
    <div v-if="mode !== 'forever'" class="nums">
      <div v-if="mode === 'days' || mode === 'both'" class="field">
        <label :for="`${idPrefix}-days`">Days</label>
        <input :id="`${idPrefix}-days`" v-model="daysText" class="input" type="number" min="1" max="36500" inputmode="numeric" @input="apply" />
      </div>
      <div v-if="mode === 'count' || mode === 'both'" class="field">
        <label :for="`${idPrefix}-count`">Newest messages</label>
        <input :id="`${idPrefix}-count`" v-model="countText" class="input" type="number" min="1" inputmode="numeric" @input="apply" />
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
