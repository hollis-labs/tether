import { test } from 'node:test'
import assert from 'node:assert/strict'
import { recipientOptions } from '../src/pages/messaging-model.ts'

test('readable recipient labels retain canonical selection values across alias edits', () => {
  const agents = [{ urn: 'msg://agent/local/stable', display_name: 'Backend' }]
  const before = recipientOptions(agents, [{ urn: agents[0].urn, alias: 'backend-old' }])
  const after = recipientOptions(agents, [{ urn: agents[0].urn, alias: 'backend-new' }])
  assert.equal(before[0].urn, after[0].urn)
  assert.equal(after[0].label, 'backend-new')
  assert.deepEqual(recipientOptions(agents, []), [{ urn: agents[0].urn, label: 'Backend' }])
})
