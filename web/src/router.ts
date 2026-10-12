import { computed, ref } from 'vue'

// A small router for a handful of screens. Paths are under /admin, so a reload or a link
// shared with someone else lands on the same screen (the server answers every path under
// /admin with the app).
export const BASE = '/admin'

export type Route = { name: 'overview' } | { name: 'inboxes' } | { name: 'tokens' } | { name: 'notfound' }

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
    default:
      return { name: 'notfound' }
  }
}

const current = ref(parse(window.location.pathname))
export const route = computed(() => current.value)

export function href(path: string): string {
  return BASE + (path === '/' ? '/' : path)
}

export function navigate(path: string): void {
  const target = href(path)
  if (target !== window.location.pathname) window.history.pushState({}, '', target)
  current.value = parse(target)
  window.scrollTo(0, 0)
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
