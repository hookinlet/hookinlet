import { ref } from 'vue'
import { getStatus, listInboxes, listTokens, type Inbox, type Status, type Token } from './api/client'
import { guard, whenSignedOut } from './session'

// Shared, loaded-on-demand state. Each piece has a value, whether a load is running, and the
// last error, so a screen can draw a skeleton, the data, or a retry. Overlapping refreshes are
// ordered: only the newest one's answer is kept, and an answer that arrives after a reset (a
// sign-out) is dropped.
function resource<T>(load: () => Promise<T>) {
  const data = ref<T | null>(null)
  const loading = ref(false)
  const error = ref('')
  let generation = 0
  let running = 0

  async function refresh() {
    const mine = ++generation
    running++
    loading.value = true
    error.value = ''
    try {
      const result = await load()
      if (mine === generation) data.value = result
    } catch (e) {
      if (mine === generation) error.value = e instanceof Error ? e.message : 'Could not load.'
    } finally {
      running--
      if (running === 0) loading.value = false
    }
  }

  function reset() {
    generation++ // whatever is still on its way is now stale
    data.value = null
    error.value = ''
  }

  whenSignedOut(reset)
  return { data, loading, error, refresh, reset }
}

export const status = resource<Status>(() => guard(getStatus))
export const inboxes = resource<Inbox[]>(() => guard(listInboxes))
export const tokens = resource<Token[]>(() => guard(listTokens))
