import { ref } from 'vue'
import { getStatus, listInboxes, listTokens, type Inbox, type Status, type Token } from './api/client'
import { guard } from './session'

// Shared, loaded-on-demand state. Each piece has a value, whether a load is running, and the
// last error, so a screen can draw a skeleton, the data, or a retry.
function resource<T>(load: () => Promise<T>) {
  const loading = ref(false)
  const error = ref('')
  const state = ref<T | null>(null)
  async function refresh() {
    loading.value = true
    error.value = ''
    try {
      state.value = await load()
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'Could not load.'
    } finally {
      loading.value = false
    }
  }
  return { data: state, loading, error, refresh }
}

export const status = resource<Status>(() => guard(getStatus))
export const inboxes = resource<Inbox[]>(() => guard(listInboxes))
export const tokens = resource<Token[]>(() => guard(listTokens))

export function reset() {
  status.data.value = null
  inboxes.data.value = null
  tokens.data.value = null
}
