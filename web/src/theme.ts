import { ref } from 'vue'

export type Theme = 'system' | 'dark' | 'light'
const KEY = 'hookinlet-theme'

function stored(): Theme {
  try {
    const v = window.localStorage.getItem(KEY)
    if (v === 'dark' || v === 'light') return v
  } catch {
    // storage can be blocked; the system theme is the fallback
  }
  return 'system'
}

export const theme = ref<Theme>(stored())

function apply(t: Theme) {
  const root = document.documentElement
  if (t === 'system') root.removeAttribute('data-theme')
  else root.setAttribute('data-theme', t)
}

export function initTheme() {
  apply(theme.value)
}

export function cycleTheme() {
  const next: Theme = theme.value === 'system' ? 'dark' : theme.value === 'dark' ? 'light' : 'system'
  theme.value = next
  apply(next)
  try {
    if (next === 'system') window.localStorage.removeItem(KEY)
    else window.localStorage.setItem(KEY, next)
  } catch {
    // not remembered; it still applies for this visit
  }
}
