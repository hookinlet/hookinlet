import { ref } from 'vue'
import { recentTimes, type Inbox } from '../api/client'
import { buckets } from '../activity'
import { guard } from '../session'

// Per-inbox activity over the last 24 hours, read from each inbox's newest messages
// (summaries only), a few inboxes at a time so a long list does not open a hundred
// requests at once. An inbox with more messages than one page holds in a day is marked
// as capped, since its count is then a floor.
export const activity = ref<Record<string, { series: number[]; capped: boolean }>>({})
const LIMIT = 200
const PARALLEL = 5
const MAX_INBOXES = 60

export async function loadActivity(list: Inbox[]): Promise<void> {
  const todo = list.filter((i) => i.last_seq > 0).slice(0, MAX_INBOXES)
  let next = 0
  async function worker() {
    for (;;) {
      const inbox = todo[next++]
      if (!inbox) return
      try {
        const times = await guard((t) => recentTimes(t, inbox.slug, inbox.last_seq, LIMIT))
        const series = buckets(times)
        const oldest = times[times.length - 1]
        const capped = times.length >= LIMIT && oldest !== undefined && Date.now() - new Date(oldest).getTime() < 24 * 3600_000
        activity.value = { ...activity.value, [inbox.slug]: { series, capped } }
      } catch {
        // an inbox that cannot be read (deleted meanwhile) just has no line
      }
    }
  }
  await Promise.all(Array.from({ length: Math.min(PARALLEL, todo.length) }, worker))
}
