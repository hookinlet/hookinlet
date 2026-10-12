<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { navigate } from '../router'
import { signOut } from '../session'
import { inboxes } from '../store'
import { cycleTheme } from '../theme'
import { filterCommands, type Command } from '../palette'
import Icon from './Icon.vue'

const open = defineModel<boolean>({ required: true })
const query = ref('')
const at = ref(0)
const input = ref<HTMLInputElement | null>(null)
let opener: HTMLElement | null = null

const commands = computed<Command[]>(() => {
  const go = (label: string, path: string): Command => ({ id: path, label, run: () => navigate(path) })
  const list: Command[] = [
    go('Go to Overview', '/'),
    go('Go to Inboxes', '/inboxes'),
    go('Go to Tokens', '/tokens'),
    go('Go to Maintenance', '/maintenance'),
    go('New inbox', '/new'),
  ]
  for (const i of inboxes.data.value ?? []) {
    list.push({ id: `open-${i.slug}`, label: `Open inbox ${i.slug}`, hint: i.name, run: () => navigate(`/inboxes/${i.slug}`) })
    list.push({ id: `set-${i.slug}`, label: `Settings of ${i.slug}`, hint: i.name, run: () => navigate(`/inboxes/${i.slug}/settings`) })
  }
  list.push({ id: 'theme', label: 'Switch theme', run: cycleTheme })
  list.push({ id: 'out', label: 'Sign out', run: () => (signOut(), navigate('/')) })
  return list
})
const results = computed(() => filterCommands(commands.value, query.value).slice(0, 12))
watch(query, () => (at.value = 0))

watch(open, async (o) => {
  if (o) {
    opener = document.activeElement as HTMLElement | null
    query.value = ''
    at.value = 0
    if (!inboxes.data.value) void inboxes.refresh() // so inboxes can be found by name
    await nextTick()
    input.value?.focus()
  } else {
    opener?.focus()
  }
})

function close() {
  open.value = false
}
function run(c: Command | undefined) {
  if (!c) return
  close()
  c.run()
}
function key(e: KeyboardEvent) {
  if (e.key === 'Escape') (e.preventDefault(), close())
  else if (e.key === 'ArrowDown') (e.preventDefault(), (at.value = Math.min(results.value.length - 1, at.value + 1)))
  else if (e.key === 'ArrowUp') (e.preventDefault(), (at.value = Math.max(0, at.value - 1)))
  else if (e.key === 'Enter') (e.preventDefault(), run(results.value[at.value]))
  else if (e.key === 'Tab') e.preventDefault() // the box is the only thing to reach while it is open
}
</script>

<template>
  <div v-if="open" class="scrim" @mousedown.self="close">
    <div class="box card" role="dialog" aria-modal="true" aria-label="Command palette" @keydown="key">
      <div class="q">
        <Icon name="search" />
        <input ref="input" v-model="query" class="qi" type="text" role="combobox" aria-expanded="true" aria-controls="palette-list" :aria-activedescendant="results[at] ? `pal-${results[at]!.id}` : undefined" placeholder="Go to, open, or do…" autocomplete="off" spellcheck="false" aria-label="Search commands" />
        <kbd>Esc</kbd>
      </div>
      <ul id="palette-list" class="results" role="listbox" aria-label="Commands">
        <li v-for="(c, i) in results" :id="`pal-${c.id}`" :key="c.id" role="option" :aria-selected="i === at" class="it" :class="{ on: i === at }" @mousemove="at = i" @click="run(c)">
          {{ c.label }}<span v-if="c.hint" class="muted hint">{{ c.hint }}</span>
        </li>
        <li v-if="!results.length" class="none muted" role="presentation">Nothing matches “{{ query }}”.</li>
      </ul>
    </div>
  </div>
</template>

<style scoped>
.scrim { position: fixed; inset: 0; background: color-mix(in srgb, var(--bg) 72%, transparent); display: grid; place-items: start center; padding: 14vh 16px 0; z-index: 50; }
.box { width: min(100%, 520px); box-shadow: var(--shadow); overflow: hidden; }
.q { display: flex; align-items: center; gap: 10px; padding: 12px 14px; border-bottom: 1px solid var(--line); color: var(--mute); }
.q svg { width: 17px; height: 17px; flex: none; }
.qi { flex: 1; background: none; border: 0; outline: none; font-size: 15px; color: var(--fg); min-width: 0; }
kbd { font: 11px var(--font-mono); border: 1px solid var(--line); border-radius: 5px; padding: 1px 5px; }
.results { list-style: none; margin: 0; padding: 6px; max-height: 50vh; overflow: auto; }
.it { display: flex; gap: 10px; align-items: baseline; padding: 8px 10px; border-radius: 8px; cursor: pointer; font-size: 14px; }
.it.on { background: var(--panel2); }
.hint { font-size: 12px; }
.none { padding: 12px; font-size: 13.5px; }
</style>
