import { z } from 'zod'

/**
 * Settings form. Mirrors validate.UpdateUserSettings on the server (name
 * 1-255 characters, image an http(s) URL); an empty image clears it.
 */
export const updateUserSettingsSchema = z.object({
  name: z.string().trim().min(1, 'Enter your name').max(255, 'Name must be at most 255 characters'),
  image: z
    .string()
    .trim()
    .transform(value => (value === '' ? null : value))
    .pipe(z.url({ protocol: /^https?$/, error: 'Enter an http(s) image URL' }).nullable()),
})

export type UpdateUserSettingsInput = z.input<typeof updateUserSettingsSchema>
