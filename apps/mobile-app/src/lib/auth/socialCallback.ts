/**
 * Pure helpers for Google/Apple sign-in (socialSignIn.ts): which providers
 * the app knows, what the provider flow's deep link carries back, and the
 * user-facing message for each failure. No React Native imports, so they
 * are unit-tested in plain Node.
 */

export const socialProviders = ['google', 'apple'] as const

export type SocialProvider = (typeof socialProviders)[number]

const providerLabels: Record<SocialProvider, string> = {
  google: 'Google',
  apple: 'Apple',
}

export function socialProviderLabel(provider: SocialProvider): string {
  return providerLabels[provider]
}

/**
 * The providers from a GET /api/auth/social-providers body
 * (`{"providers": ["google", "apple"]}`), in the app's order. Unknown names
 * and malformed bodies are dropped, so the buttons only ever show providers
 * the app can render.
 */
export function parseSocialProviders(body: unknown): SocialProvider[] {
  const listed = (body as { providers?: unknown } | null)?.providers
  if (!Array.isArray(listed)) return []
  return socialProviders.filter(provider => listed.includes(provider))
}

export type CallbackResult = { code: string } | { error: string }

/**
 * Reads the deep link the provider flow returns to: `?code=<one-time code>`
 * on success, `?error=<snake_case code>` on failure. Anything else is an
 * error, so a link without a code never counts as signed in.
 */
export function parseCallbackURL(url: string): CallbackResult {
  const query = url.split('#')[0].split('?')[1] ?? ''
  const params = new URLSearchParams(query)
  const error = params.get('error')
  if (error) return { error }
  const code = params.get('code')
  if (code) return { code }
  return { error: 'invalid_code' }
}

/**
 * The message to show for an `?error=` code from the callback, or null when
 * there is nothing to show (the user declined at the provider).
 */
export function callbackErrorMessage(error: string, provider: SocialProvider): string | null {
  const name = socialProviderLabel(provider)
  switch (error) {
    case 'access_denied':
      return null
    case 'account_not_linked':
      return 'An account with this email already exists. Sign in with your email and password.'
    case 'email_not_verified':
      return 'Verify your email address, then try again.'
    case 'signup_disabled':
      return 'New accounts cannot be created right now.'
    case 'banned':
      return 'This account cannot sign in.'
    case 'two_factor_required':
      return 'This account uses two-factor authentication. Sign in with your email and password.'
    case 'email_not_found':
      return `${name} did not share an email address with us.`
    case 'state_mismatch':
    case 'please_restart_the_process':
    case 'invalid_code':
      return `${name} sign-in expired. Try again.`
    default:
      return `Could not sign in with ${name}. Try again.`
  }
}

/** HTTP statuses the auth endpoints answer social sign-in with. */
export const HttpStatus = {
  badRequest: 400, // INVALID_TOKEN: a spent, expired or mismatched handoff code
  unauthorized: 401,
  notFound: 404, // PROVIDER_NOT_FOUND: not configured on the server
  tooManyRequests: 429,
  serverError: 500,
} as const

/** The message for a failed start or code exchange request, by HTTP status. */
export function requestErrorMessage(status: number, provider: SocialProvider): string {
  const name = socialProviderLabel(provider)
  if (status === HttpStatus.tooManyRequests) return 'Too many attempts. Wait a moment and try again.'
  if (status === HttpStatus.notFound) return `${name} sign-in is not available.`
  if (status === HttpStatus.badRequest || status === HttpStatus.unauthorized) return `${name} sign-in expired. Try again.`
  return `Could not sign in with ${name}. Try again.`
}
