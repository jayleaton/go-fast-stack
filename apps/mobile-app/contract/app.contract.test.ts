import { randomUUID } from 'node:crypto'

import { beforeAll, describe, expect, it, vi } from 'vitest'

import * as keychain from './stubs/expo-secure-store'

/**
 * The app's own clients against the real Go server: the Better Auth Expo
 * client (src/lib/auth/authClient.ts) for /api/auth/*, and apiFetch via
 * userService for /api/core/v1/users/me. Nothing here talks HTTP directly
 * except where noted, so a passing run proves the shipped code works.
 */

/** Imports the app modules fresh, like a cold app launch reading the keychain. */
async function launchApp() {
  vi.resetModules()
  const auth = await import('@/lib/auth/authClient')
  const users = await import('@/lib/services/userService')
  const { apiFetch } = await import('@/lib/api/client')
  return { ...auth, ...users, apiFetch }
}

const password = 'correct-horse-battery'
const newEmail = () => `mobile-${randomUUID()}@example.com`

// One sequential flow per user: the auth endpoints are rate limited per IP.
describe('mobile app against the Go server', () => {
  const email = newEmail()
  let app: Awaited<ReturnType<typeof launchApp>>

  beforeAll(async () => {
    keychain.clearAll()
    app = await launchApp()
  })

  it('signs up and stores the session cookie in the keychain', async () => {
    const { data, error } = await app.signUp.email({ name: 'Ada', email, password })
    expect(error).toBeNull()
    expect(data?.user.email).toBe(email)
    expect(await app.authClient.getCookie()).toMatch(/^better-auth\.session_token=/)
  })

  it('gets the session with the stored cookie', async () => {
    const { data, error } = await app.authClient.getSession()
    expect(error).toBeNull()
    expect(data?.user.email).toBe(email)
    expect(data?.session.userId).toBe(data?.user.id)
  })

  it('signs out, clearing the server session and the keychain', async () => {
    const { error } = await app.signOut()
    expect(error).toBeNull()
    expect(await app.authClient.getCookie()).toBe('')
    expect((await app.authClient.getSession()).data).toBeNull()
  })

  it('rejects a wrong password with Better Auth’s error code', async () => {
    const { data, error } = await app.signIn.email({ email, password: 'wrong-password-123' })
    expect(data).toBeNull()
    expect(error?.status).toBe(401)
    expect(error?.code).toBe('INVALID_EMAIL_OR_PASSWORD')
  })

  it('signs in and calls GET /api/core/v1/users/me with the session', async () => {
    const { error } = await app.signIn.email({ email, password })
    expect(error).toBeNull()

    const me = await app.getCurrentUser()
    expect(me.error).toBeNull()
    expect(me.data?.user).toMatchObject({ email, name: 'Ada', emailVerified: false, image: null })
    expect(me.data?.session.userId).toBe(me.data?.user.id)
  })

  it('updates settings with PATCH /api/core/v1/users/me', async () => {
    const updated = await app.updateUserSettings({ name: 'Ada Lovelace', image: 'https://example.com/ada.png' })
    expect(updated.error).toBeNull()
    expect(updated.data?.user).toMatchObject({ name: 'Ada Lovelace', image: 'https://example.com/ada.png' })

    const cleared = await app.updateUserSettings({ image: null })
    expect(cleared.data?.user).toMatchObject({ name: 'Ada Lovelace', image: null })

    expect((await app.getCurrentUser()).data?.user.name).toBe('Ada Lovelace')
  })

  it('surfaces validation and permission errors as envelope errors', async () => {
    const invalid = await app.updateUserSettings({ name: '' })
    expect(invalid.data).toBeNull()
    expect(invalid.error).toEqual(expect.any(String))

    const { userPath } = await import('@/lib/config/routes')
    const other = await app.apiFetch(userPath('someone-else'), {
      method: 'PATCH',
      body: JSON.stringify({ name: 'Mallory' }),
    })
    expect(other.data).toBeNull()
    expect(other.error).toEqual(expect.any(String))
  })

  it('lists the sessions with GET /api/core/v1/users/me/sessions', async () => {
    const me = await app.getCurrentUser()
    const { data, error } = await app.listSessions()
    expect(error).toBeNull()
    // Signed in once (the sign-up session was signed out), so one session: this one.
    expect(data).toHaveLength(1)
    expect(data?.[0]).toMatchObject({ id: me.data?.session.id, current: true })
  })

  it('restores the session on the next launch from the keychain', async () => {
    // The splash screen waits on this atom. expoClient fills it from the
    // keychain cache as the first get-session starts (better-auth 1.7: in that
    // request's init, before it is sent), then get-session confirms it. Hold
    // the auth client's requests back to prove the cached session is there
    // without the network. The client keeps the fetch it was created with.
    const sendFetch = globalThis.fetch
    let release = () => {}
    const held = new Promise<void>(resolve => (release = resolve))
    let sent = 0
    globalThis.fetch = async (input, init) => {
      await held
      sent++
      return sendFetch(input, init)
    }
    let relaunched: Awaited<ReturnType<typeof launchApp>>
    try {
      relaunched = await launchApp()
    } finally {
      globalThis.fetch = sendFetch
    }
    const pending = relaunched.authClient.getSession()
    try {
      await vi.waitFor(() => expect(relaunched.authClient.$store.atoms.session.get().data?.user.email).toBe(email))
      expect(sent).toBe(0)
    } finally {
      release()
    }
    const { data } = await pending
    expect(sent).toBe(1)
    expect(data?.user.name).toBe('Ada Lovelace')
    expect((await relaunched.getCurrentUser()).data?.user.email).toBe(email)
    app = relaunched
  })

  it('signs out and the API answers Unauthorized', async () => {
    await app.signOut()
    const me = await app.getCurrentUser()
    expect(me).toEqual({ data: null, error: 'Unauthorized' })
  })
})

describe('delete account', () => {
  it('DELETE /api/core/v1/users/me deletes the user, clears the local session and blocks sign-in', async () => {
    keychain.clearAll()
    const app = await launchApp()
    const email = newEmail()
    expect((await app.signUp.email({ name: 'Temp', email, password })).error).toBeNull()

    const deleted = await app.deleteAccount()
    expect(deleted).toEqual({ data: { success: true }, error: null })
    expect(await app.authClient.getCookie()).toBe('')
    expect((await app.authClient.getSession()).data).toBeNull()

    const again = await app.signIn.email({ email, password })
    expect(again.error?.code).toBe('INVALID_EMAIL_OR_PASSWORD')
  })
})
