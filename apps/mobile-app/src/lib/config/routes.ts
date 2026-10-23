/**
 * Centralized navigation routes to avoid magic strings across the codebase.
 * Paths are validated against Expo Router's typed routes (experiments.typedRoutes).
 */
export const AppRoutes = {
  signIn: '/sign-in',
  signUp: '/sign-up',
  dashboard: '/dashboard',
  /** Where Google/Apple sign-in returns (as a deep link built with Linking.createURL). */
  authCallback: '/auth/callback',
} as const

/**
 * Better Auth endpoints (packages/auth) the app calls with authClient.$fetch,
 * relative to the client's /api/auth base. The client's own methods
 * (signIn.email, getSession, ...) carry their paths themselves.
 */
export const AuthRoutes = {
  socialProviders: '/social-providers',
  signInSocial: '/sign-in/social',
  signInHandoff: '/sign-in/handoff',
} as const

/**
 * The Go JSON API endpoints the app calls, as the same patterns as APIRoutes
 * in apps/web/internal/config/routes.go (`{id}` placeholders included).
 * routes.test.ts fails when one of them is missing on the Go side. Build
 * concrete paths with the helpers below.
 */
export const ApiRoutes = {
  users: {
    me: '/api/core/v1/users/me',
    byId: '/api/core/v1/users/{id}',
    meSessions: '/api/core/v1/users/me/sessions',
  },
} as const

/** Concrete path for ApiRoutes.users.byId. */
export function userPath(id: string): string {
  return ApiRoutes.users.byId.replace('{id}', encodeURIComponent(id))
}
