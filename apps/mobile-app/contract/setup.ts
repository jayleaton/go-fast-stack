import { inject } from 'vitest'

// src/lib/config/env.ts reads this when the app modules are imported.
process.env.EXPO_PUBLIC_API_URL = inject('apiUrl')
