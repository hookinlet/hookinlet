export interface Command {
  id: string
  label: string
  hint?: string
  run: () => void
}

// score says how well a query matches a label: 0 for no match, higher for a better one. A match
// at the start of the label or of a word beats one in the middle, and a run of letters beats
// letters scattered through it.
export function score(label: string, query: string): number {
  const q = query.trim().toLowerCase()
  if (q === '') return 1
  const l = label.toLowerCase()
  const at = l.indexOf(q)
  if (at === 0) return 100
  if (at > 0) return /[\s\-/._]/.test(l[at - 1] ?? '') ? 80 : 60
  // letters in order, not next to each other
  let i = 0
  for (const ch of l) if (ch === q[i]) i++
  return i === q.length ? 20 : 0
}

export function filterCommands(commands: Command[], query: string): Command[] {
  return commands
    .map((c, i) => ({ c, s: Math.max(score(c.label, query), c.hint ? score(c.hint, query) / 2 : 0), i }))
    .filter((x) => x.s > 0)
    .sort((a, b) => b.s - a.s || a.i - b.i)
    .map((x) => x.c)
}
