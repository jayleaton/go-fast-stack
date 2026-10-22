import { authClient } from '@/lib/auth/authClient'
import { ApiError, parseEnvelope } from '@/lib/api/envelope'
import type { ApiResponse } from '@/lib/api/types'
import { env } from '@/lib/config/env'

/**
 * Fetch wrapper for the Go JSON API (/api/core/v1).
 *
 * Sends the Better Auth session cookie from SecureStore with every request
 * and always resolves to the `{ data, error }` envelope. Only unreachable
 * networks throw (ApiError); HTTP and validation failures arrive as envelope
 * errors, like on the web. A string body is sent as JSON.
 */
export async function apiFetch<T>(path: string, init?: RequestInit): Promise<ApiResponse<T>> {
  const cookie = authClient.getCookie()

  let response: Response
  try {
    response = await fetch(`${env.apiUrl}${path}`, {
      ...init,
      headers: {
        ...(typeof init?.body === 'string' ? { 'Content-Type': 'application/json' } : {}),
        ...(init?.headers ?? {}),
        ...(cookie ? { Cookie: cookie } : {}),
      },
      // Cookies are set explicitly above; let the OS handle the rest.
      credentials: 'omit',
    })
  } catch {
    throw new ApiError(
      `Could not reach the API at ${env.apiUrl}. Is the Go server running and EXPO_PUBLIC_API_URL correct for this device?`,
      0
    )
  }

  const body = await response.json().catch(() => null)
  const envelope = parseEnvelope<T>(body)

  if (!response.ok && !envelope.error) {
    return { data: null, error: `Request failed: ${response.status} ${response.statusText}` }
  }

  return envelope
}
