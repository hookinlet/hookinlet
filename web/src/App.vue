<script setup lang="ts">
import { computed } from 'vue'
import AppShell from './components/AppShell.vue'
import SignIn from './components/SignIn.vue'
import { href, link, route } from './router'
import { signedIn } from './session'
import InboxesView from './views/InboxesView.vue'
import InboxView from './views/InboxView.vue'
import OverviewView from './views/OverviewView.vue'
import TokensView from './views/TokensView.vue'

const view = computed(() => {
  switch (route.value.name) {
    case 'overview':
      return OverviewView
    case 'inboxes':
      return InboxesView
    case 'tokens':
      return TokensView
    default:
      return null
  }
})
</script>

<template>
  <SignIn v-if="!signedIn" />
  <AppShell v-else>
    <InboxView v-if="route.name === 'inbox'" :key="route.slug" :slug="route.slug" :seq="route.seq" />
    <component :is="view" v-else-if="view" :key="route.name" />
    <section v-else class="card missing">
      <h1>Not found</h1>
      <p class="muted">There is no such page in the console.</p>
      <a :href="href('/')" @click="link($event, '/')">Back to the overview</a>
    </section>
  </AppShell>
</template>

<style scoped>
.missing { padding: 28px; display: grid; gap: 8px; justify-items: start; max-width: 28rem; }
h1 { font-size: 20px; }
</style>
