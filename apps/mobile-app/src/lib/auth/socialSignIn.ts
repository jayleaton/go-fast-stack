import * as Linking from 'expo-linking'
import * as WebBrowser from 'expo-web-browser'

import { authClient } from '@/lib/auth/authClient'
import {
  callbackErrorMessage,
  HttpStatus,
  parseCallbackURL,
  parseSocialProviders,
  requestErrorMessage,
  type SocialProvider,
} from '@/lib/auth/socialCallback'
import { AppRoutes, AuthRoutes } from '@/lib/config/routes'

export type { SocialProvider } from '@/lib/auth/socialCallback'
export { socialProviderLabel } from '@/lib/auth/socialCallback'

/**
 * The providers the server has configured, for the "Continue with ..."
 * buttons. Empty when none are, or when the server cannot be asked: the
 * buttons are optional, email sign-in still works.
 */
export async function getSocialProviders(): Promise<SocialProvider[]> {
  try {
    const { data } = await authClient.$fetch<unknown>(AuthRoutes.socialProviders, { method: 'GET' })
    return parseSocialProviders(data)
  } catch {
    return []
  }
}

type StartResponse = { url?: string; handoffSecret?: string }

/**
 * Signs in with Google or Apple, resolving to a message to show, or null on
 * success or when the user cancelled. On success the session cookie is in
 * SecureStore and useSession updates, exactly as after email sign-in.
 *
 * The flow never puts a session token in a URL: the server sends the
 * browser back to the app's deep link with a one-time code (single use,
 * 60 s), which only works together with the handoff secret from the start
 * request. The secret stays in this function's memory and is never stored
 * or logged.
 */
export async function signInWithProvider(provider: SocialProvider): Promise<{ error: string | null }> {
  // betterstack://auth/callback in builds, exp://<host>/--/auth/callback in Expo Go.
  const callbackURL = Linking.createURL(AppRoutes.authCallback.replace(/^\//, ''))

  try {
    // The server answers with redirect:false, so the expoClient plugin does
    // not open its own browser flow for this /sign-in/* response.
    const start = await authClient.$fetch<StartResponse>(AuthRoutes.signInSocial, {
      method: 'POST',
      body: { provider, callbackURL },
    })
    if (start.error) return { error: requestErrorMessage(start.error.status, provider) }
    const { url, handoffSecret } = start.data ?? {}
    if (!url || !handoffSecret) return { error: requestErrorMessage(HttpStatus.serverError, provider) }

    const result = await WebBrowser.openAuthSessionAsync(url, callbackURL)
    if (result.type !== 'success') return { error: null }

    const callback = parseCallbackURL(result.url)
    if ('error' in callback) return { error: callbackErrorMessage(callback.error, provider) }

    // Same response as /sign-in/email: the expoClient plugin stores its
    // Set-Cookie in SecureStore and notifies the session signal, so
    // useSession refetches and the (auth) layout redirects.
    const exchange = await authClient.$fetch(AuthRoutes.signInHandoff, {
      method: 'POST',
      body: { code: callback.code, handoffSecret },
    })
    if (exchange.error) return { error: requestErrorMessage(exchange.error.status, provider) }
    return { error: null }
  } catch {
    return { error: 'Could not reach the server. Check your connection and try again.' }
  }
}
