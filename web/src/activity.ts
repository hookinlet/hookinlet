export const HOURS = 24

// buckets counts timestamps into the HOURS hours before `now`, oldest first, for a
// sparkline. A timestamp outside that window, or in the future, is not counted.
export function buckets(times: string[], now: Date = new Date()): number[] {
  const out = new Array<number>(HOURS).fill(0)
  const hour = 3600_000
  for (const s of times) {
    const age = now.getTime() - new Date(s).getTime()
    if (Number.isNaN(age) || age < 0 || age >= HOURS * hour) continue
    out[HOURS - 1 - Math.floor(age / hour)]! += 1
  }
  return out
}
