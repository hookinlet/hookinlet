<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import InboxTable from '../components/InboxTable.vue'
import { inboxes } from '../store'
import { loadActivity, loadingActivity } from './useActivity'

const q = ref('')
const shown = computed(() => {
  const needle = q.value.trim().toLowerCase()
  const all = inboxes.data.value ?? []
  return needle ? all.filter((i) => i.slug.includes(needle) || i.name.toLowerCase().includes(needle) || i.verifier.includes(needle)) : all
})

onMounted(async () => {
  await inboxes.refresh()
  if (inboxes.data.value) void loadActivity(inboxes.data.value)
})
</script>

<template>
  <header class="top">
    <h1>Inboxes</h1>
    <span class="count muted num" v-if="inboxes.data.value">{{ inboxes.data.value.length }}</span>
    <span class="grow"></span>
    <label class="sr-only" for="q">Filter inboxes</label>
    <input id="q" v-model="q" class="input filter" type="search" placeholder="Filter by name, slug or verifier" autocomplete="off" />
  </header>
  <p v-if="inboxes.error.value" class="err" role="alert">{{ inboxes.error.value }}</p>
  <InboxTable v-if="inboxes.data.value && shown.length" :inboxes="shown" :loading-lines="loadingActivity" />
  <section v-else-if="inboxes.data.value" class="card empty">
    <h2>{{ q ? 'No inbox matches' : 'No inboxes yet' }}</h2>
    <p class="muted">{{ q ? 'Try a different word.' : 'Create one with hookinlet inbox create or the admin API.' }}</p>
  </section>
  <div v-else class="card skel"><div class="skeleton line"></div><div class="skeleton line"></div><div class="skeleton line"></div></div>
</template>

<style scoped>
.top { display: flex; align-items: center; gap: 12px; margin-bottom: 20px; flex-wrap: wrap; }
h1 { font-size: 22px; }
.grow { flex: 1; }
.filter { width: min(100%, 280px); }
.empty { padding: 28px; display: grid; gap: 8px; }
.empty h2 { font-size: 17px; }
.skel { padding: 16px; display: grid; gap: 14px; }
.line { height: 2.2rem; }
</style>
