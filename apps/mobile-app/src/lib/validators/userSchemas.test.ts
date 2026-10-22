import { describe, expect, it } from 'vitest'

import { updateUserSettingsSchema } from './userSchemas'

describe('updateUserSettingsSchema', () => {
  it('trims the name and turns an empty image into null (clears it)', () => {
    expect(updateUserSettingsSchema.parse({ name: '  Ada  ', image: '  ' })).toEqual({ name: 'Ada', image: null })
  })

  it('keeps an http(s) image URL', () => {
    const image = 'https://example.com/a.png'
    expect(updateUserSettingsSchema.parse({ name: 'Ada', image })).toEqual({ name: 'Ada', image })
  })

  it('rejects what the server rejects', () => {
    expect(updateUserSettingsSchema.safeParse({ name: ' ', image: '' }).success).toBe(false)
    expect(updateUserSettingsSchema.safeParse({ name: 'x'.repeat(256), image: '' }).success).toBe(false)
    expect(updateUserSettingsSchema.safeParse({ name: 'Ada', image: 'javascript:alert(1)' }).success).toBe(false)
    expect(updateUserSettingsSchema.safeParse({ name: 'Ada', image: 'ftp://example.com/a.png' }).success).toBe(false)
  })
})
