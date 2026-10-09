// Run with browser_run_code_unsafe(code=contents of this file) against the owned
// Vite fixture on 15177. Every API request is intercepted; no state DB is used.
async (page) => {
  const base = 'http://127.0.0.1:15177'
  await page.unroute(`${base}/api/**`)
  let read = false
  let delayed = false
  let releaseRead
  let readStarted
  const queries = []
  let profile = { urn: 'msg://user/local/chris', aliases: [], messaging: {} }
  let profileError = false
  let delayNextProfile = false
  let profileStarted
  let releaseProfile
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
    if (url.pathname === '/api/messages/profile') {
      if (profileError) {
        await route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: JSON.stringify({ message: 'invalid saved messaging.from_default in user profile' }),
        })
        return
      }
      if (request.method() === 'POST') {
        const submitted = request.postDataJSON()
        if (Object.keys(submitted).join(',') !== 'from_default')
          throw new Error('client chose preference key or identity')
        const value =
          submitted.from_default === '@chris-mux'
            ? 'msg://user/agent-mux/chris'
            : submitted.from_default
        profile = { ...profile, messaging: value ? { from_default: value } : {} }
      }
      body = profile
      if (request.method() === 'GET' && delayNextProfile) {
        delayNextProfile = false
        body = structuredClone(profile)
        profileStarted?.()
        await new Promise(resolve => { releaseProfile = resolve })
      }
    } else if (url.pathname === '/api/messages/read') {
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
  await page
    .getByRole('button', { name: 'Open message from local/test', exact: true })
    .waitFor({ state: 'hidden' })
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
  const compose = page.getByRole('dialog')
  const from = compose.getByLabel('From', { exact: true })
  if ((await from.inputValue()) !== profile.urn)
    throw new Error('configured local user default ignored')
  await from.fill('@chris-mux')
  await compose.getByRole('button', { name: 'Save From default', exact: true }).click()
  await compose.getByText('From default saved', { exact: true }).waitFor()
  if ((await from.inputValue()) !== 'msg://user/agent-mux/chris')
    throw new Error('saved alias did not become canonical')
  await page.reload()
  await page.getByRole('button', { name: 'New Message', exact: true }).click()
  await page.getByRole('dialog').getByLabel('From', { exact: true }).waitFor()
  // Wait for the profile-backed value, rather than relying on response timing.
  await page.waitForFunction(
    () =>
      document.querySelector('input[aria-label="From"]')?.value === 'msg://user/agent-mux/chris',
  )
  await page.getByRole('dialog').getByRole('button', { name: 'Close', exact: true }).click()
  const filteredResponse = page.waitForResponse(
    (r) =>
      r.url().includes('/api/messages?') &&
      new URL(r.url()).searchParams.get('to') === '@chris-mux',
  )
  await page.getByLabel('Recipient filter', { exact: true }).fill('@chris-mux')
  await filteredResponse
  delayNextProfile = true
  const staleStarted = new Promise(resolve => { profileStarted = resolve })
  await page.getByRole('button', { name: 'Refresh', exact: true }).click()
  await staleStarted
  await page.getByRole('button', { name: 'New Message', exact: true }).click()
  await page.getByRole('dialog').getByLabel('From', { exact: true }).fill('msg://user/local/chris')
  await page.getByRole('dialog').getByRole('button', { name: 'Save From default', exact: true }).click()
  await page.getByRole('dialog').getByText('From default saved', { exact: true }).waitFor()
  await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent.trim() === 'Refresh' && !button.disabled))
  const staleResponse = page.waitForResponse(r => r.url().endsWith('/api/messages/profile') && r.request().method() === 'GET')
  releaseProfile()
  await staleResponse
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
  if (await page.getByRole('dialog').getByLabel('From', { exact: true }).inputValue() !== 'msg://user/local/chris')
    throw new Error('older profile read restored the previous saved default')
  profileError = true
  await page.reload()
  await page
    .getByText('invalid saved messaging.from_default in user profile', { exact: true })
    .waitFor()
  return {
    localUserDefault: true,
    delayedProfileReadCannotUndoSave: true,
    canonicalSavedDefaultSurvivesReload: true,
    invalidSavedDefaultSurfaced: true,
    optionalRecipientFilter: true,
    unreadDetailRetained: true,
    replyAvailable: true,
    delayedReadUsesLatestScopeFilterAndPage: true,
    recipientLabelVisible: true,
  }
}
