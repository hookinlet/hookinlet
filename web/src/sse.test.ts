import { describe, expect, it } from 'vitest'
import { SSEParser } from './sse'

describe('SSEParser', () => {
  it('reads the server’s own framing', () => {
    const p = new SSEParser()
    const got = p.push('retry: 3000\n\nid: 7\nevent: message\ndata: {"seq":7}\n\n: ping\n\nevent: gap\ndata: {"after":1}\n\n')
    expect(p.retry).toBe(3000)
    expect(got).toEqual([
      { event: 'message', id: '7', data: '{"seq":7}' },
      { event: 'gap', id: '7', data: '{"after":1}' }, // the id carries on until another is set
    ])
  })
  it('joins events split across chunks, and \\r\\n line ends', () => {
    const p = new SSEParser()
    expect(p.push('id: 1\r\nevent: mess')).toEqual([])
    expect(p.push('age\r\ndata: a\r\n')).toEqual([])
    expect(p.push('data: b\r\n\r\n')).toEqual([{ event: 'message', id: '1', data: 'a\nb' }])
  })
  it('does not split a \\r\\n that straddles two chunks', () => {
    const p = new SSEParser()
    expect(p.push('data: x\r')).toEqual([])
    expect(p.push('\n\r\n')).toEqual([{ event: 'message', id: '', data: 'x' }])
  })
  it('ignores comments, unknown fields and a bad retry', () => {
    const p = new SSEParser()
    expect(p.push(': hi\nfoo: bar\nretry: soon\n\n')).toEqual([])
    expect(p.retry).toBeNull()
  })
})
