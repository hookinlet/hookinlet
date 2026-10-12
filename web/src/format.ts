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
