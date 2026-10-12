import { computed, ref } from 'vue'

// A small router for a handful of screens. Paths are under /admin, so a reload or a link
// shared with someone else lands on the same screen (the server answers every path under
// /admin with the app).
export const BASE = '/admin'

export type Route =
  | { name: 'overview' }
  | { name: 'inboxes' }
  | { name: 'inbox'; slug: string; seq: number | null; tab: 'messages' | 'settings' }
  | { name: 'newinbox' }
  | { name: 'tokens' }
  | { name: 'maintenance' }
  | { name: 'notfound' }

export function parse(pathname: string): Route {
  const p = pathname.startsWith(BASE) ? pathname.slice(BASE.length) : pathname
  const clean = p.replace(/\/+$/, '') || '/'
  switch (clean) {
    case '/':
      return { name: 'overview' }
    case '/inboxes':
      return { name: 'inboxes' }
    case '/tokens':
      return { name: 'tokens' }
    case '/maintenance':
      return { name: 'maintenance' }
    case '/new':
      return { name: 'newinbox' }
    default: {
      const s = /^\/inboxes\/([a-z0-9-]{1,64})\/settings$/.exec(clean)
      if (s) return { name: 'inbox', slug: s[1]!, seq: null, tab: 'settings' }
      const m = /^\/inboxes\/([a-z0-9-]{1,64})(?:\/messages\/(\d{1,15}))?$/.exec(clean)
      if (m) return { name: 'inbox', slug: m[1]!, seq: m[2] ? Number(m[2]) : null, tab: 'messages' }
      return { name: 'notfound' }
    }
  }
}

const current = ref(parse(window.location.pathname))
export const route = computed(() => current.value)

export function href(path: string): string {
  return BASE + (path === '/' ? '/' : path)
}

export function navigate(path: string): void {
  const target = href(path)
  const before = current.value
  if (target !== window.location.pathname) window.history.pushState({}, '', target)
  current.value = parse(target)
  // Moving between the messages of one inbox keeps the page where it is; any other move starts at the top.
  const after = current.value
  const sameInbox = before.name === 'inbox' && after.name === 'inbox' && before.slug === after.slug && before.tab === after.tab
  if (!sameInbox) window.scrollTo(0, 0)
}

window.addEventListener('popstate', () => {
  current.value = parse(window.location.pathname)
})

// link is the click handler for an <a href>: it navigates in the page unless the person
// is opening the link some other way (a new tab, a modifier key).
export function link(e: MouseEvent, path: string): void {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
  e.preventDefault()
  navigate(path)
}
