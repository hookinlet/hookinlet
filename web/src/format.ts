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

// visible makes the characters that change how text is laid out or that cannot be seen show as
// <U+XXXX>, so that a sender cannot use them to make an event type, a header or a path read as
// something it is not: every default-ignorable code point (zero-width and soft hyphens, fillers,
// variation selectors, invisible operators), the bidirectional controls, the other format
// characters, and the line and paragraph separators.
const STRICT = /[\p{Default_Ignorable_Code_Point}\p{Bidi_Control}\p{Cf}\p{Zl}\p{Zp}]/gu

// In a message body the joiners and variation selectors that make up emoji and many scripts are
// text and stay as they are. Only what can reorder the text or hide it is marked: the
// bidirectional controls, the line and paragraph separators, and the tag characters.
const IN_BODY = /[\p{Bidi_Control}\u2028\u2029\u{e0000}-\u{e007f}]/gu

const mark = (c: string) => `<U+${c.codePointAt(0)!.toString(16).toUpperCase().padStart(4, '0')}>`

export function visible(text: string): string {
  return text.replace(STRICT, mark)
}

// visibleInBody is visible for the text of a message body.
export function visibleInBody(text: string): string {
  return text.replace(IN_BODY, mark)
}
