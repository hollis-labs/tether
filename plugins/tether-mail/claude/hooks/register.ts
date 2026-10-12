import type { Register } from 'claude-code'

export const lines = () => {
  let rest = ''
  return (text: string): any[] => {
    const parts = (rest + text).split('\n')
    rest = parts.pop() || ''
    if (rest.length > 1024 * 1024) throw new Error('Outpost frame too large')
    return parts.filter(Boolean).map(line => JSON.parse(line))
  }
}

// Mod loader permits relative imports + claude-code only. Socket operations
// use its sanctioned local-process API, not node:net.
export const accept = async ($: any, binary: string, socket: string, frame: any) => {
  let error = ''
  try { await $.prompt.submit({ text: frame.text }) } catch (e) { error = String(e) }
  const result = await $.process.run([binary, 'mail', '--socket', socket, 'ack'], {
    stdin: JSON.stringify({ id: frame.id, capability: frame.capability, cmd_id: frame.cmd_id, error }), timeoutMs: 6000,
  })
  if (result.exitCode !== 0) throw new Error(result.stderr || 'Outpost ack failed')
}

export const register: Register = (on, options) => {
  let stopped = false
  on('session.start', async ($, event, next) => {
    const socket = String(options.socket || (await $.env.get('TETHER_MAIL_SOCKET')) || '')
    if (!socket) { $.ui.status('Outpost mail: configure socket or TETHER_MAIL_SOCKET'); return next(event) }
    const binary = String(options.binary || 'tether-worker')
    const urn = String(options.urn || (await $.env.get('TETHER_AGENT_URN')) || '')
    const name = String(options.name || 'claude')
    const argv = [binary, 'mail', '--socket', socket, 'attach', '--adapter', 'claude-mod', '--name', name, '--handle', 'prompt-submit']
    if (urn) argv.push('--urn', urn)
    for (const sender of String(options.allowSenders || '').split(',').map(s => s.trim()).filter(Boolean)) argv.push('--allow-sender', sender)
    void (async () => {
      while (!stopped) {
        const split = lines()
        try {
          for await (const chunk of $.process.spawn({ argv })) {
            if (stopped) break
            if (chunk.stream !== 'stdout') continue
            for (const frame of split(chunk.text)) {
              if (frame.op === 'registered') $.ui.status(undefined)
              if (frame.op === 'deliver') await accept($, binary, socket, frame)
            }
          }
        } catch (e) { $.ui.status(`Outpost mail: ${e}`) }
        if (!stopped) await $.clock.sleep(1000)
      }
    })()
    return next(event)
  })
  on('session.end', ($, event, next) => { stopped = true; return next(event) })
}
