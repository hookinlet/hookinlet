// A Server-Sent Events parser for the stream endpoint. The browser's EventSource cannot send
// the Authorization header, so the console reads the stream with fetch and parses it here.
export interface SSEEvent {
  event: string
  id: string
  data: string
}

export class SSEParser {
  private buffer = ''
  private event = ''
  private id = ''
  private data: string[] = []
  // retry is the reconnection delay the server asked for, in milliseconds, once it has said so.
  retry: number | null = null

  // push takes the next piece of text from the stream and returns the events it completed.
  push(chunk: string): SSEEvent[] {
    this.buffer += chunk
    const out: SSEEvent[] = []
    for (;;) {
      const m = /\r\n|\r|\n/.exec(this.buffer)
      if (!m) break
      // A lone \r at the very end may be the first half of \r\n: wait for more.
      if (m[0] === '\r' && m.index === this.buffer.length - 1) break
      const line = this.buffer.slice(0, m.index)
      this.buffer = this.buffer.slice(m.index + m[0].length)
      const ev = this.line(line)
      if (ev) out.push(ev)
    }
    return out
  }

  private line(line: string): SSEEvent | null {
    if (line === '') {
      if (this.data.length === 0 && this.event === '') return null
      const ev = { event: this.event || 'message', id: this.id, data: this.data.join('\n') }
      this.event = ''
      this.data = []
      return ev
    }
    if (line.startsWith(':')) return null // a comment: the server's heartbeat
    const i = line.indexOf(':')
    const field = i === -1 ? line : line.slice(0, i)
    let value = i === -1 ? '' : line.slice(i + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    switch (field) {
      case 'event':
        this.event = value
        break
      case 'data':
        this.data.push(value)
        break
      case 'id':
        if (!value.includes('\0')) this.id = value
        break
      case 'retry':
        if (/^\d+$/.test(value)) this.retry = Number(value)
        break
    }
    return null
  }
}
