import { createAuthClient } from 'better-auth/react'
import { expoClient } from '@better-auth/expo/client'
import * as SecureStore from 'expo-secure-store'

import { env } from '@/lib/config/env'

/**
 * Better Auth client pointed at the Go server's auth endpoints (/api/auth/*,
 * packages/auth), which keep Better Auth's paths, JSON shapes and cookie
 * format. The expoClient plugin stores the session cookie in the device
 * keychain (expo-secure-store) and attaches it, plus the `expo-origin`
 * header the server's origin check trusts, to auth requests.
 */
export const authClient = createAuthClient({
  baseURL: env.apiUrl,
  plugins: [
    expoClient({
      scheme: 'betterstack',
      storagePrefix: 'better-stack',
      storage: SecureStore,
    }),
  ],
})

export const { signIn, signUp, signOut, useSession } = authClient

export type Session = typeof authClient.$Infer.Session
export type User = typeof authClient.$Infer.Session.user
