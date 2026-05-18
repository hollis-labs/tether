import { createApiContext } from '@hollis-labs/sysop-ui'
import { apiClient } from './client'

// Typed { ApiProvider, useApi } bound to this app's concrete client.
// Pages read the client with `useApi()`; see pages/dashboard.tsx.
export const { ApiProvider, useApi } = createApiContext(apiClient)
