const UNITS = ['B', 'KB', 'MB', 'GB', 'TB']

// bytes writes a size the way an operator reads it: 1.8 GB, not 1932735283.
export function bytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '-'
  let i = 0
  let v = n
  while (v >= 1000 && i < UNITS.length - 1) {
    v /= 1000
    i++
  }
  return `${i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)} ${UNITS[i]}`
}

// compact writes a count briefly: 4.2M, 88K, 341.
export function compact(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '-'
  if (n < 1000) return String(n)
  const units: [number, string][] = [
    [1e9, 'B'],
    [1e6, 'M'],
    [1e3, 'K'],
  ]
  for (const [i, [size, suffix]] of units.entries()) {
    if (n >= size) {
      const v = n / size
      const text = v < 10 ? v.toFixed(1).replace(/\.0$/, '') : String(Math.round(v))
      const bigger = units[i - 1]
      if (bigger && Number(text) >= 1000) return `1${bigger[1]}` // 999,999 is 1M, not 1000K
      return `${text}${suffix}`
    }
  }
  return String(n)
}

// ago writes how long before `now` a moment was: "just now", "14 min ago", "3 days ago".
export function ago(then: Date | string, now: Date = new Date()): string {
  const t = typeof then === 'string' ? new Date(then) : then
  const s = Math.round((now.getTime() - t.getTime()) / 1000)
  if (Number.isNaN(s)) return '-'
  if (s < 5) return 'just now'
  if (s < 60) return `${s} s ago`
  const m = Math.round(s / 60)
  if (m < 60) return `${m} min ago`
  const h = Math.round(m / 60)
  if (h < 48) return `${h} h ago`
  return `${Math.round(h / 24)} days ago`
}
