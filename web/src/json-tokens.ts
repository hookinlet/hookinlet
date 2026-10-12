// A JSON pretty-printer that returns tokens, not markup, so the page draws each piece as text
// and nothing a sender wrote is ever parsed as HTML.
export type TokenKind = 'key' | 'str' | 'num' | 'lit' | 'punct' | 'space'
export interface Token {
  kind: TokenKind
  text: string
}

const MAX_TOKENS = 20000

// tokens lays value out with two-space indents. It stops after MAX_TOKENS and says so, since a
// message body can be as large as the inbox allows. truncated is true when it stopped early.
export function tokens(value: unknown): { tokens: Token[]; truncated: boolean } {
  const out: Token[] = []
  let truncated = false
  const push = (kind: TokenKind, text: string) => {
    if (out.length >= MAX_TOKENS) {
      truncated = true
      return false
    }
    out.push({ kind, text })
    return true
  }
  const nl = (depth: number) => push('space', '\n' + '  '.repeat(depth))

  function walk(v: unknown, depth: number): boolean {
    if (v === null) return push('lit', 'null')
    switch (typeof v) {
      case 'string':
        return push('str', JSON.stringify(v))
      case 'number':
        return push('num', JSON.stringify(v))
      case 'boolean':
        return push('lit', String(v))
      case 'object': {
        if (Array.isArray(v)) {
          if (v.length === 0) return push('punct', '[]')
          if (!push('punct', '[')) return false
          for (let i = 0; i < v.length; i++) {
            if (!nl(depth + 1) || !walk(v[i], depth + 1)) return false
            if (i < v.length - 1 && !push('punct', ',')) return false
          }
          return nl(depth) && push('punct', ']')
        }
        const keys = Object.keys(v as Record<string, unknown>)
        if (keys.length === 0) return push('punct', '{}')
        if (!push('punct', '{')) return false
        for (let i = 0; i < keys.length; i++) {
          const k = keys[i]!
          if (!nl(depth + 1) || !push('key', JSON.stringify(k)) || !push('punct', ': ')) return false
          if (!walk((v as Record<string, unknown>)[k], depth + 1)) return false
          if (i < keys.length - 1 && !push('punct', ',')) return false
        }
        return nl(depth) && push('punct', '}')
      }
      default:
        return push('lit', String(v))
    }
  }
  walk(value, 0)
  return { tokens: out, truncated }
}
