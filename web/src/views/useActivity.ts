import { ref } from 'vue'
import { recentTimes, type Inbox } from '../api/client'
import { buckets } from '../activity'
import { guard, whenSignedOut } from '../session'

// Per-inbox activity over the last 24 hours, read from each inbox's newest messages
// (summaries only), a few inboxes at a time so a long list does not open a hundred
// requests at once. An inbox with more messages than one page holds in a day is marked
// as capped, since its count is then a floor. An inbox that was not loaded (past the
// limit, or unreadable) has no entry, and the screens say so instead of drawing a flat line.
export const activity = ref<Record<string, { series: number[]; capped: boolean }>>({})
// True when some inbox with messages has no line, so a total over the lines is a floor.
export const incomplete = ref(false)
export const loadingActivity = ref(false)

const LIMIT = 200
const PARALLEL = 5
const MAX_INBOXES = 60
let generation = 0

whenSignedOut(() => {
  generation++
  activity.value = {}
  incomplete.value = false
})

export async function loadActivity(list: Inbox[]): Promise<void> {
  const mine = ++generation
  loadingActivity.value = true
  const withMessages = list.filter((i) => i.last_seq > 0)
  const todo = withMessages.slice(0, MAX_INBOXES)
  const next: Record<string, { series: number[]; capped: boolean }> = {} // only the inboxes that exist now
  let failed = false
  let index = 0

  async function worker() {
    for (;;) {
      const inbox = todo[index++]
      if (!inbox || mine !== generation) return
      try {
        const times = await guard((t) => recentTimes(t, inbox.slug, inbox.last_seq, LIMIT))
        const oldest = times[times.length - 1]
        const capped = times.length >= LIMIT && oldest !== undefined && Date.now() - new Date(oldest).getTime() < 24 * 3600_000
        next[inbox.slug] = { series: buckets(times), capped }
        if (mine === generation) activity.value = { ...next } // lines appear as they arrive
      } catch {
        failed = true // an inbox that cannot be read (deleted meanwhile) has no line
      }
    }
  }
  await Promise.all(Array.from({ length: Math.min(PARALLEL, todo.length) }, worker))
  if (mine !== generation) return
  loadingActivity.value = false
  activity.value = { ...next }
  incomplete.value = failed || withMessages.length > todo.length
}
