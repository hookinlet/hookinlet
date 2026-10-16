<script setup lang="ts">
// What a delivery that fails verification gets: refused (the default) or kept as not verified. The wording
// is shared by the new-inbox form and the settings page. With the verifier none nothing is checked, so there
// is nothing to choose and the group says so instead.
const onFail = defineModel<'reject' | 'store'>({ required: true })
const props = defineProps<{ verifier: string; idPrefix: string }>()
</script>

<template>
  <fieldset v-if="props.verifier !== 'none'" class="field">
    <legend class="label">When verification fails</legend>
    <label class="radio"><input v-model="onFail" type="radio" :name="`${props.idPrefix}-onfail`" value="reject" /> Refuse it: the sender gets a 401 and nothing is stored <span class="muted">(default)</span></label>
    <label class="radio"><input v-model="onFail" type="radio" :name="`${props.idPrefix}-onfail`" value="store" /> Keep it, marked not verified: the sender gets a 200 and stops retrying</label>
    <p v-if="onFail === 'store'" class="muted help">An inbox keeps a limited number of unverified messages (1000 by default). Past that, deliveries get a 429 until some are deleted.</p>
  </fieldset>
  <p v-else class="muted help">This inbox checks nothing, so no delivery fails: every one is stored as not verified.</p>
</template>

<style scoped>
fieldset { border: 0; margin: 0; padding: 0; display: grid; gap: 8px; min-width: 0; }
.radio { display: flex; gap: 8px; align-items: center; font-size: 13.5px; cursor: pointer; }
.help { font-size: 12.5px; }
</style>
