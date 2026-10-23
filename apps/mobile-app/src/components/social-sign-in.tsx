import { useEffect, useState } from 'react'
import { StyleSheet, View } from 'react-native'

import { AppButton } from '@/components/app-button'
import { ThemedText } from '@/components/themed-text'
import { Spacing } from '@/constants/theme'
import {
  getSocialProviders,
  signInWithProvider,
  socialProviderLabel,
  type SocialProvider,
} from '@/lib/auth/socialSignIn'

/**
 * "Continue with Google / Apple" buttons for the sign-in and sign-up
 * screens. Shows only the providers the server has configured and renders
 * nothing when there are none. On success the (auth) layout redirects.
 */
export function SocialSignIn() {
  const [providers, setProviders] = useState<SocialProvider[]>([])
  const [pending, setPending] = useState<SocialProvider | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let active = true
    getSocialProviders().then(list => {
      if (active) setProviders(list)
    })
    return () => {
      active = false
    }
  }, [])

  if (providers.length === 0) return null

  const handlePress = async (provider: SocialProvider) => {
    setError(null)
    setPending(provider)
    const result = await signInWithProvider(provider)
    setPending(null)
    setError(result.error)
  }

  return (
    <View style={styles.container}>
      <ThemedText themeColor="textSecondary" style={styles.divider}>
        or
      </ThemedText>
      {providers.map(provider => (
        <AppButton
          key={provider}
          variant="secondary"
          label={`Continue with ${socialProviderLabel(provider)}`}
          loading={pending === provider}
          disabled={pending !== null}
          onPress={() => handlePress(provider)}
        />
      ))}
      {error ? <ThemedText style={styles.error}>{error}</ThemedText> : null}
    </View>
  )
}

/** Form error text, as on the sign-in and sign-up screens. */
const errorColor = '#e5484d'
const errorFontSize = 14

const styles = StyleSheet.create({
  container: {
    gap: Spacing.three,
  },
  divider: {
    textAlign: 'center',
  },
  error: {
    color: errorColor,
    fontSize: errorFontSize,
  },
})
