import { describe, expect, it } from 'vitest'

import { callbackErrorMessage, parseCallbackURL, parseSocialProviders } from './socialCallback'

describe('parseCallbackURL', () => {
  it.each([
    ['betterstack://auth/callback?code=abc123', { code: 'abc123' }],
    ['exp://192.168.1.2:8081/--/auth/callback?code=abc123#', { code: 'abc123' }],
    ['betterstack://auth/callback?error=account_not_linked', { error: 'account_not_linked' }],
    ['betterstack://auth/callback?code=abc&error=state_mismatch', { error: 'state_mismatch' }],
    ['betterstack://auth/callback', { error: 'invalid_code' }],
    ['betterstack://auth/callback?code=', { error: 'invalid_code' }],
  ])('%s', (url, want) => {
    expect(parseCallbackURL(url)).toEqual(want)
  })
})

describe('parseSocialProviders', () => {
  it('keeps known providers in the app order and drops the rest', () => {
    expect(parseSocialProviders({ providers: ['apple', 'github', 'google'] })).toEqual(['google', 'apple'])
    expect(parseSocialProviders({ providers: [] })).toEqual([])
    expect(parseSocialProviders(null)).toEqual([])
    expect(parseSocialProviders({ providers: 'google' })).toEqual([])
  })
})

describe('callbackErrorMessage', () => {
  it('shows nothing when the user declined and names the provider otherwise', () => {
    expect(callbackErrorMessage('access_denied', 'google')).toBeNull()
    expect(callbackErrorMessage('internal_server_error', 'apple')).toBe('Could not sign in with Apple. Try again.')
  })
})
