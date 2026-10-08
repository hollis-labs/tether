// Run with browser_run_code_unsafe(code=contents of this file) against the owned
// Vite fixture on 15177. Every API request is intercepted; no state DB is used.
;async (page) => {
  const base = 'http://127.0.0.1:15177'
  await page.unroute(`${base}/api/**`)
  let read = false
  let delayed = false
  let releaseRead
  let readStarted
  const queries = []
  const totals = {
    total: 102,
    user: { total: 1, unread: 1, archived: 0 },
    agent: { total: 101, unread: 0, archived: 0 },
    groups: { total: 0, unread: 0, archived: 0 },
  }
  await page.route(`${base}/api/**`, async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    let body = {}
    if (url.pathname === '/api/messages/read') {
      readStarted?.()
      if (delayed)
        await new Promise((resolve) => {
          releaseRead = resolve
        })
      read = true
      body = { status: 'ok' }
    } else if (url.pathname === '/api/messages' && request.method() === 'GET') {
      const query = Object.fromEntries(url.searchParams)
      queries.push(query)
      const user = query.scope === 'user'
      const hidden = user && read && query.read === 'unread'
      const row = {
        id: user ? 'user1' : `agent-${query.offset}`,
        kind: 'notice',
        from: 'msg://agent/local/test',
        to: user ? 'msg://user/local/chris' : 'msg://agent/local/recipient',
        scope: query.scope,
        subject: user ? 'Fixture inbox message' : `Agent page ${query.offset}`,
        body: 'Fixture body',
        created_at: '2026-10-08T00:00:00Z',
        ...(read && user ? { read_at: '2026-10-08T01:00:00Z' } : {}),
      }
      body = {
        messages: hidden ? [] : [row],
        total: hidden ? 0 : user ? 1 : 101,
        limit: 100,
        offset: Number(query.offset),
        totals,
      }
    } else if (url.pathname.endsWith('/agents')) {
      body = {
        agents: [
          { urn: 'msg://agent/local/stable', display_name: 'Backend label', status: 'active' },
        ],
      }
    } else if (url.pathname.endsWith('/aliases')) body = { aliases: [] }
    else if (url.pathname.endsWith('/groups')) body = { groups: [] }
    else if (url.pathname.includes('/broker/')) body = { envelopes: [] }
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(body),
    })
  })

  await page.goto(`${base}/messaging`)
  await page.getByRole('button', { name: 'Open message from local/test', exact: true }).waitFor()
  const unreadResponse = page.waitForResponse(
    (r) =>
      r.url().includes('/api/messages?') && new URL(r.url()).searchParams.get('read') === 'unread',
  )
  await page.getByLabel('Read state', { exact: true }).selectOption('unread')
  await unreadResponse
  const readRefresh = page.waitForResponse((r) => read && r.url().includes('/api/messages?'))
  await page.getByRole('button', { name: 'Open message from local/test', exact: true }).click()
  await readRefresh
  const dialog = page.getByRole('dialog')
  await dialog.waitFor()
  if (!(await dialog.getByText('Fixture body', { exact: true }).isVisible()))
    throw new Error('read removed the selected detail')
  await dialog.getByRole('textbox').fill('Reply after read')
  if (!(await dialog.getByRole('button', { name: 'Send reply', exact: true }).isEnabled()))
    throw new Error('reply unavailable after unread row leaves the page')
  await dialog.getByRole('button', { name: 'Close', exact: true }).click()
  await page.getByText('No messages', { exact: true }).waitFor()

  read = false
  delayed = true
  await page.reload()
  await page.getByRole('button', { name: 'Open message from local/test', exact: true }).waitFor()
  await page.getByLabel('Read state', { exact: true }).selectOption('unread')
  const started = new Promise((resolve) => {
    readStarted = resolve
  })
  await page.getByRole('button', { name: 'Open message from local/test', exact: true }).click()
  await started
  await page.getByRole('dialog').getByRole('button', { name: 'Close', exact: true }).click()
  await page.getByRole('button', { name: /^Agents / }).click()
  await page.getByLabel('Read state', { exact: true }).selectOption('read')
  await page.getByLabel('Archive state', { exact: true }).selectOption('all')
  await page.getByRole('button', { name: 'Next', exact: true }).click()
  await page.getByText('Agent page 100', { exact: true }).waitFor()
  const afterChange = queries.length
  const completed = page.waitForResponse((r) => r.url().endsWith('/api/messages/read'))
  const latestRefresh = page.waitForResponse(
    (r) =>
      r.url().includes('/api/messages?') && new URL(r.url()).searchParams.get('offset') === '100',
  )
  releaseRead()
  await completed
  await latestRefresh
  await page.getByText('Agent page 100', { exact: true }).waitFor()
  for (const q of queries.slice(afterChange)) {
    if (q.scope !== 'agent' || q.read !== 'read' || q.archive !== 'all' || q.offset !== '100')
      throw new Error(`stale mutation query: ${JSON.stringify(q)}`)
  }
  if (await page.getByRole('dialog').count())
    throw new Error('delayed read reopened a closed dialog')
  await page.getByRole('button', { name: 'New Message', exact: true }).click()
  const recipient = page.getByLabel('Recipient', { exact: true })
  await recipient.selectOption('msg://agent/local/stable')
  if ((await recipient.locator('option:checked').innerText()) !== 'Backend label')
    throw new Error('recipient control displays the URN instead of its label')
  return {
    unreadDetailRetained: true,
    replyAvailable: true,
    delayedReadUsesLatestScopeFilterAndPage: true,
    recipientLabelVisible: true,
  }
}
