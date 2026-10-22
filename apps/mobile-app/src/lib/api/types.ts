/**
 * Types for the Go JSON API (/api/core/v1). They mirror the Go response
 * structs in apps/web/internal/api (users.go, route.go) and the user model in
 * packages/database/model; keep them in sync when those change.
 */

/** Every JSON API response: `{ data, error }` (api.WriteData / api.WriteError). */
export type ApiResponse<T> = {
  data: T | null
  error: string | null
}

/** services.User, serialized. Timestamps are RFC 3339 strings. */
export type ApiUser = {
  id: string
  name: string
  email: string
  emailVerified: boolean
  image: string | null
  createdAt: string
  updatedAt: string
}

/** api.Session: the caller's session as the API exposes it. */
export type ApiSession = {
  id: string
  userId: string
  expiresAt: string
}

/** GET /api/core/v1/users/me */
export type CurrentUser = { user: ApiUser; session: ApiSession }

/** PATCH /api/core/v1/users/me and /api/core/v1/users/{id} */
export type UserUpdated = { success: boolean; user: ApiUser }

/** DELETE /api/core/v1/users/me */
export type UserDeleted = { success: boolean }

/**
 * validate.UpdateUserSettings: omitted fields are unchanged, `image: null`
 * clears the image.
 */
/** services.Session: one of the user's sessions (GET /api/core/v1/users/me/sessions). */
export type UserSession = {
  id: string
  createdAt: string
  expiresAt: string
  ipAddress: string | null
  userAgent: string | null
  /** the session that made the request */
  current: boolean
}

export type UpdateUserSettingsBody = {
  name?: string
  image?: string | null
}
