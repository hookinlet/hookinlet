<script setup lang="ts">
import { ref } from 'vue'
import Icon from './Icon.vue'

const props = defineProps<{ value: string; label: string }>()
const copied = ref(false)
const field = ref<HTMLElement | null>(null)

async function copy() {
  try {
    await navigator.clipboard.writeText(props.value)
  } catch {
    // The clipboard can be refused; select the text so the person can copy it themselves.
    if (field.value) {
      const range = document.createRange()
      range.selectNodeContents(field.value)
      const sel = window.getSelection()
      sel?.removeAllRanges()
      sel?.addRange(range)
    }
    return
  }
  copied.value = true
  window.setTimeout(() => (copied.value = false), 1600)
}
</script>

<template>
  <div class="copy">
    <span ref="field" class="mono value" role="group" :aria-label="label">{{ value }}</span>
    <button type="button" class="btn quiet" @click="copy">
      <Icon :name="copied ? 'check' : 'copy'" />
      <span aria-live="polite">{{ copied ? 'Copied' : 'Copy' }}</span>
    </button>
  </div>
</template>

<style scoped>
.copy { display: flex; align-items: center; gap: 8px; background: var(--panel2); border: 1px solid var(--line); border-radius: 8px; padding: 4px 4px 4px 12px; min-width: 0; }
.value { flex: 1; min-width: 0; overflow-wrap: anywhere; user-select: all; font-size: 12.5px; padding-block: 4px; }
</style>
