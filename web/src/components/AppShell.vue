<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { bytes } from '../format'
import { link, navigate, route, href } from '../router'
import { signOut } from '../session'
import { status } from '../store'
import { cycleTheme, theme } from '../theme'
import CommandPalette from './CommandPalette.vue'
import Icon from './Icon.vue'

const nav = [
  { path: '/', name: 'overview', label: 'Overview', icon: 'overview' },
  { path: '/inboxes', name: 'inboxes', label: 'Inboxes', icon: 'inboxes' },
  { path: '/tokens', name: 'tokens', label: 'Tokens', icon: 'tokens' },
  { path: '/maintenance', name: 'maintenance', label: 'Maintenance', icon: 'wrench' },
] as const

const themeIcon = computed(() => (theme.value === 'dark' ? 'moon' : theme.value === 'light' ? 'sun' : 'monitor'))
const free = computed(() => (status.data.value?.free_bytes === undefined ? '' : `${bytes(status.data.value.free_bytes)} free`))

function out() {
  signOut()
  navigate('/')
}

const palette = ref(false)
function onKey(e: KeyboardEvent) {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault()
    palette.value = !palette.value
  }
}
onMounted(() => window.addEventListener('keydown', onKey))
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))

const main = ref<HTMLElement | null>(null)
// A page change is announced by moving focus to the new page, for screen readers and keyboards.
watch(
  () => route.value.name,
  () => void nextTick(() => main.value?.focus({ preventScroll: true })),
)

onMounted(() => void status.refresh())
</script>

<template>
  <div class="shell">
    <aside class="side">
      <a class="logo" :href="href('/')" @click="link($event, '/')"><i aria-hidden="true"></i>hookinlet</a>
      <button type="button" class="search" @click="palette = true"><Icon name="search" />Search<kbd>⌘K</kbd></button>
      <nav aria-label="Main">
        <a v-for="n in nav" :key="n.name" class="nav" :class="{ on: route.name === n.name }" :href="href(n.path)" :aria-current="route.name === n.name ? 'page' : undefined" @click="link($event, n.path)">
          <Icon :name="n.icon" />{{ n.label }}
        </a>
      </nav>
      <div class="grow"></div>
      <section class="srv" aria-label="Server">
        <span class="row"><span class="dot" :class="{ off: !status.data.value }" aria-hidden="true"></span>{{ status.data.value ? 'ready' : 'checking' }}</span>
        <template v-if="status.data.value">
          <span class="mono">{{ /^\d/.test(status.data.value.version) ? 'v' : '' }}{{ status.data.value.version }} · schema {{ status.data.value.schema_version }}</span>
          <span>db {{ bytes(status.data.value.database_bytes) }}<template v-if="free"> · {{ free }}</template></span>
        </template>
      </section>
      <div class="tools">
        <button type="button" class="btn quiet" :title="`Theme: ${theme}`" @click="cycleTheme"><Icon :name="themeIcon" /><span class="sr-only">Theme: {{ theme }}. Change.</span></button>
        <button type="button" class="btn quiet" @click="out"><Icon name="logout" />Sign out</button>
      </div>
    </aside>
    <main ref="main" class="main" tabindex="-1"><slot /></main>
    <CommandPalette v-model="palette" />
  </div>
</template>

<style scoped>
.shell { display: grid; grid-template-columns: 220px minmax(0, 1fr); min-height: 100%; }
.side { position: sticky; top: 0; align-self: start; height: 100vh; background: var(--panel); border-right: 1px solid var(--line); padding: 16px 12px; display: flex; flex-direction: column; gap: 4px; }
.logo { display: flex; align-items: center; gap: 9px; padding: 4px 8px 18px; font-weight: 600; font-size: 16px; letter-spacing: -0.02em; color: var(--fg); }
.logo:hover { text-decoration: none; }
.logo i { width: 22px; height: 22px; border: 2px solid var(--fg); border-top-color: transparent; border-radius: 0 0 6px 6px; position: relative; display: inline-block; }
.logo i::after { content: ''; position: absolute; left: 4px; top: 3px; width: 8px; height: 4px; border-radius: 2px; background: var(--accent); }
.search { display: flex; align-items: center; gap: 10px; width: 100%; margin-bottom: 10px; padding: 7px 10px; border: 1px solid var(--line); border-radius: 8px; background: var(--bg); color: var(--mute); cursor: pointer; font-size: 13px; }
.search:hover { color: var(--fg); }
.search svg { width: 15px; height: 15px; }
.search kbd { margin-left: auto; font: 11px var(--font-mono); }
nav { display: flex; flex-direction: column; gap: 2px; }
.nav { display: flex; align-items: center; gap: 10px; padding: 8px 10px; border-radius: 8px; color: var(--mute); font-weight: 500; }
.nav:hover { background: var(--panel2); color: var(--fg); text-decoration: none; }
.nav.on { background: var(--panel2); color: var(--fg); }
.nav svg { width: 17px; height: 17px; flex: none; }
.grow { flex: 1; }
.srv { margin: 0 4px 8px; padding: 10px; border: 1px solid var(--line); border-radius: 8px; font-size: 12px; color: var(--mute); display: grid; gap: 3px; }
.srv .mono { color: var(--fg); font-size: 11.5px; }
.row { display: flex; align-items: center; gap: 7px; color: var(--fg); font-weight: 500; }
.dot { width: 7px; height: 7px; border-radius: 50%; background: var(--ok); }
.dot.off { background: var(--mute); }
.tools { display: flex; justify-content: space-between; }
.tools .btn { padding: 6px 8px; }
.main { padding: 28px 32px 56px; min-width: 0; max-width: 1180px; width: 100%; }
.main:focus { outline: none; }

@media (max-width: 760px) {
  .shell { grid-template-columns: minmax(0, 1fr); }
  .side { position: static; height: auto; flex-direction: row; flex-wrap: wrap; align-items: center; gap: 4px 8px; padding: 10px 12px; border-right: 0; border-bottom: 1px solid var(--line); }
  .logo { padding: 0 8px 0 0; }
  nav { flex-direction: row; order: 3; width: 100%; overflow-x: auto; }
  .grow, .srv { display: none; }
  .search { width: auto; margin: 0 0 0 auto; padding: 6px 10px; }
  .search kbd { display: none; }
  .tools { margin-left: auto; gap: 4px; }
  .main { padding: 20px 16px 48px; }
}
</style>
