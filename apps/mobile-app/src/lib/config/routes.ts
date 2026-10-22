/**
 * Centralized navigation routes to avoid magic strings across the codebase.
 * Paths are validated against Expo Router's typed routes (experiments.typedRoutes).
 */
export const AppRoutes = {
  signIn: '/sign-in',
  signUp: '/sign-up',
  dashboard: '/dashboard',
} as const

/**
 * Centralized API routes, served by the Go server (apps/web).
 * Keep in sync with APIRoutes in apps/web/internal/config/routes.go.
 */
export const ApiRoutes = {
  users: {
    current: '/api/core/v1/users',
    byId: (id: string) => `/api/core/v1/users/${encodeURIComponent(id)}`,
  },
} as const
