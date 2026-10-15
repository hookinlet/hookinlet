<script setup lang="ts">
import { computed } from 'vue'
import { presetOf, PRESETS } from '../verifiers'

// The choice of how deliveries are checked, and the settings that choice has. The values live in
// the parent, so the new-inbox form and the settings page share one set of fields.
const verifier = defineModel<string>('verifier', { required: true })
const values = defineModel<Record<string, string>>('values', { required: true })
const secret = defineModel<string>('secret', { default: '' })
const props = defineProps<{ withSecret: boolean; idPrefix: string }>()

const preset = computed(() => presetOf(verifier.value))
function pick(id: string) {
  verifier.value = id
  values.value = {}
}
function set(key: string, v: string) {
  values.value = { ...values.value, [key]: v }
}
</script>

<template>
  <div class="vf">
    <fieldset class="field">
      <legend class="label">Who is sending</legend>
      <div class="chips" role="radiogroup" aria-label="Verification">
        <label v-for="p in PRESETS" :key="p.id" class="chip" :class="{ on: verifier === p.id }" :title="p.summary">
          <input type="radio" :name="`${props.idPrefix}-verifier`" :value="p.id" :checked="verifier === p.id" class="sr-only" @change="pick(p.id)" />
          {{ p.label }}
        </label>
      </div>
      <p class="muted help">{{ preset?.summary }}</p>
      <p v-if="verifier === 'none'" class="callout" role="note"><strong>Verification is off.</strong> Anyone who has the ingest URL can post anything, and every message is stored as not verified. That suits testing, or a sender that cannot sign; pick a verifier before real traffic.</p>
    </fieldset>

    <div v-if="withSecret && preset && preset.secret" class="field">
      <label :for="`${idPrefix}-secret`">{{ preset.secret.label }}</label>
      <input :id="`${idPrefix}-secret`" v-model="secret" class="input mono" type="password" autocomplete="off" spellcheck="false" :placeholder="preset.secret.placeholder" />
      <p class="muted help">Stored sealed under the master key. It is never shown again.</p>
    </div>

    <div v-for="f in preset?.fields ?? []" :key="f.key" class="field">
      <label :for="`${idPrefix}-${f.key}`">{{ f.label }}<span v-if="f.required" class="req"> (required)</span></label>
      <select v-if="f.kind === 'select'" :id="`${idPrefix}-${f.key}`" class="input" :value="values[f.key] ?? ''" @change="set(f.key, ($event.target as HTMLSelectElement).value)">
        <option v-for="o in f.options" :key="o" :value="o">{{ o || 'Default' }}</option>
      </select>
      <input v-else :id="`${idPrefix}-${f.key}`" class="input" :class="{ mono: f.kind === 'text' }" :type="f.kind === 'number' ? 'number' : 'text'" :min="f.kind === 'number' ? 1 : undefined" autocomplete="off" spellcheck="false" :placeholder="f.placeholder" :value="values[f.key] ?? ''" @input="set(f.key, ($event.target as HTMLInputElement).value)" />
      <p class="muted help">{{ f.help }}</p>
    </div>
  </div>
</template>

<style scoped>
.vf { display: grid; gap: 16px; }
fieldset { border: 0; margin: 0; padding: 0; display: grid; gap: 8px; min-width: 0; }
.chips { display: flex; gap: 6px; flex-wrap: wrap; }
.chip { border: 1px solid var(--field); background: var(--panel); border-radius: 8px; padding: 6px 12px; font-size: 13px; color: var(--mute); cursor: pointer; }
.chip:hover { color: var(--fg); }
.chip.on { border-color: var(--accent); color: var(--accent); background: var(--accent-soft); }
.chip:focus-within { outline: 2px solid var(--accent); outline-offset: 2px; }
.help { font-size: 12.5px; }
.req { color: var(--warn); }
</style>
