import { test, expect } from 'claude-code/testing'
import { accept, lines } from './register'

test('socket framing joins arbitrary stream chunks', () => {
  const split = lines()
  expect(split('{"op":"reg')).toEqual([])
  expect(split('istered"}\n')).toEqual([{ op: 'registered' }])
})

test('native prompt acceptance precedes credential-free local ack', async () => {
  const order: string[] = []
  let input: any
  const $ = {
    prompt: { submit: async ({ text }: { text: string }) => { order.push(text) } },
    process: { run: async (argv: string[], init: any) => { order.push('ack'); input = JSON.parse(init.stdin); expect(argv).toEqual(['tether-worker', 'mail', '--socket', '/tmp/mail.sock', 'ack']); return { exitCode: 0 } } },
  }
  await accept($, 'tether-worker', '/tmp/mail.sock', { id: 'reg', capability: 'secret', cmd_id: 'mail', text: 'data body' })
  expect(order).toEqual(['data body', 'ack'])
  expect(input).toEqual({ id: 'reg', capability: 'secret', cmd_id: 'mail', error: '' })
})

test('failed prompt submission is a failed delivery, not a success receipt', async () => {
  let error = ''
  const $ = { prompt: { submit: async () => { throw new Error('busy') } }, process: { run: async (_: any, init: any) => { error = JSON.parse(init.stdin).error; return { exitCode: 0 } } } }
  await accept($, 'tether-worker', '/tmp/mail.sock', { id: 'reg', capability: 'secret', cmd_id: 'mail', text: 'data' })
  expect(error).toContain('busy')
})
