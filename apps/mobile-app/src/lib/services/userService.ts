import { apiFetch } from '@/lib/api/client'
import type {
  ApiResponse,
  CurrentUser,
  UpdateUserSettingsBody,
  UserDeleted,
  UserUpdated,
} from '@/lib/api/types'
import { signOut } from '@/lib/auth/authClient'
import { ApiRoutes } from '@/lib/config/routes'

/**
 * Calls GET /api/core/v1/users: the canonical example of an authenticated,
 * envelope-shaped API call from the mobile app.
 */
export function getCurrentUser(): Promise<ApiResponse<CurrentUser>> {
  return apiFetch<CurrentUser>(ApiRoutes.users.current)
}

/** PATCH /api/core/v1/users: updates the signed-in user's settings. */
export function updateUserSettings(body: UpdateUserSettingsBody): Promise<ApiResponse<UserUpdated>> {
  return apiFetch<UserUpdated>(ApiRoutes.users.current, {
    method: 'PATCH',
    body: JSON.stringify(body),
  })
}

/**
 * DELETE /api/core/v1/users: deletes the signed-in user and all their data.
 * On success the server has already revoked every session, so the local
 * session is cleared too (sign-out clears SecureStore even if the server no
 * longer knows the session).
 */
export async function deleteAccount(): Promise<ApiResponse<UserDeleted>> {
  const result = await apiFetch<UserDeleted>(ApiRoutes.users.current, { method: 'DELETE' })
  if (result.data?.success) {
    await signOut()
  }
  return result
}
