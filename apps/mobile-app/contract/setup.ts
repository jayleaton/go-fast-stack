import { inject } from 'vitest'

import { nativeFetch } from './nativeFetch'

// The app's clients call the global fetch: send it as the native app does,
// without the Fetch Metadata Node's fetch adds (see nativeFetch.ts).
globalThis.fetch = nativeFetch

// src/lib/config/env.ts reads this when the app modules are imported.
process.env.EXPO_PUBLIC_API_URL = inject('apiUrl')
