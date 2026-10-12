<script setup lang="ts">
import { computed } from 'vue'
import { tokens } from '../json-tokens'

// A pretty-printed JSON value, drawn token by token as text. Nothing here is ever parsed as
// HTML, because the value was written by whoever sent the webhook.
const props = defineProps<{ value: unknown }>()
const laid = computed(() => tokens(props.value))
</script>

<template>
  <div>
    <pre class="json"><template v-for="(t, i) in laid.tokens" :key="i"><span :class="t.kind">{{ t.text }}</span></template></pre>
    <p v-if="laid.truncated" class="muted note">This body is large, so only the start is drawn. Use the Raw tab for every byte.</p>
  </div>
</template>

<style scoped>
.json { margin: 0; font: 12.5px/1.65 var(--font-mono); white-space: pre-wrap; overflow-wrap: anywhere; }
.key { color: var(--code-key); }
.str { color: var(--code-str); }
.num { color: var(--code-num); }
.lit { color: var(--code-num); }
.punct, .space { color: var(--mute); }
.note { margin-top: 8px; font-size: 12.5px; }
</style>
