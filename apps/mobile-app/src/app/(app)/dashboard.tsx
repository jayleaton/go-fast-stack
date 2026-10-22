import { useCallback, useEffect, useState } from 'react'
import { Alert, RefreshControl, ScrollView, StyleSheet, View } from 'react-native'

import { AppButton } from '@/components/app-button'
import { AppTextInput } from '@/components/app-text-input'
import { Screen } from '@/components/screen'
import { ThemedText } from '@/components/themed-text'
import { ThemedView } from '@/components/themed-view'
import { Spacing } from '@/constants/theme'
import type { ApiResponse, CurrentUser } from '@/lib/api/types'
import { signOut, useSession } from '@/lib/auth/authClient'
import { deleteAccount, getCurrentUser, updateUserSettings } from '@/lib/services/userService'
import { getFieldErrors } from '@/lib/validators/formErrors'
import { updateUserSettingsSchema } from '@/lib/validators/userSchemas'

/**
 * Protected home screen. Doubles as the canonical example of calling the Go
 * envelope API with the mobile session: GET /api/core/v1/users/me renders the
 * data card, PATCH saves settings and DELETE removes the account. Failures
 * render the envelope error or the unreachable-network ApiError.
 */
export default function DashboardScreen() {
  const { data: session } = useSession()
  const [envelope, setEnvelope] = useState<ApiResponse<CurrentUser> | null>(null)
  const [fetchError, setFetchError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  const [name, setName] = useState('')
  const [image, setImage] = useState('')
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [settingsMessage, setSettingsMessage] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [deleting, setDeleting] = useState(false)

  const showResult = useCallback((result: ApiResponse<CurrentUser>) => {
    setEnvelope(result)
    setFetchError(null)
    if (result.data) {
      setName(result.data.user.name)
      setImage(result.data.user.image ?? '')
    }
  }, [])

  const showFailure = useCallback((error: unknown) => {
    setEnvelope(null)
    setFetchError(error instanceof Error ? error.message : 'Something went wrong')
  }, [])

  // First load; state is only set once the request settles.
  useEffect(() => {
    let active = true
    getCurrentUser()
      .then(result => active && showResult(result), error => active && showFailure(error))
      .finally(() => active && setLoading(false))
    return () => {
      active = false
    }
  }, [showResult, showFailure])

  const fetchCurrentUser = async () => {
    setLoading(true)
    try {
      showResult(await getCurrentUser())
    } catch (error) {
      showFailure(error)
    } finally {
      setLoading(false)
    }
  }

  const handleSave = async () => {
    const parsed = updateUserSettingsSchema.safeParse({ name, image })
    if (!parsed.success) {
      setFieldErrors(getFieldErrors(parsed.error))
      return
    }

    setFieldErrors({})
    setSettingsMessage(null)
    setSaving(true)
    try {
      const result = await updateUserSettings(parsed.data)
      if (result.data) {
        setEnvelope(prev => (prev?.data ? { ...prev, data: { ...prev.data, user: result.data!.user } } : prev))
        setSettingsMessage('Settings saved')
      } else {
        setSettingsMessage(result.error)
      }
    } catch (error) {
      setSettingsMessage(error instanceof Error ? error.message : 'Something went wrong')
    } finally {
      setSaving(false)
    }
  }

  const confirmDelete = () => {
    Alert.alert('Delete account', 'This permanently deletes your account and all its data.', [
      { text: 'Cancel', style: 'cancel' },
      {
        text: 'Delete',
        style: 'destructive',
        onPress: async () => {
          setDeleting(true)
          try {
            const result = await deleteAccount()
            // On success the session is gone and the (app) layout redirects.
            if (result.error) {
              Alert.alert('Could not delete account', result.error)
            }
          } catch (error) {
            Alert.alert('Could not delete account', error instanceof Error ? error.message : 'Something went wrong')
          } finally {
            setDeleting(false)
          }
        },
      },
    ])
  }

  const apiUser = envelope?.data?.user
  const busy = loading || saving || deleting

  return (
    <Screen>
      <ScrollView
        contentContainerStyle={styles.content}
        keyboardShouldPersistTaps="handled"
        refreshControl={<RefreshControl refreshing={loading} onRefresh={fetchCurrentUser} />}
      >
        <ThemedText type="title">Dashboard</ThemedText>
        <ThemedText themeColor="textSecondary" style={styles.subtitle}>
          Signed in as {session?.user.email}
        </ThemedText>

        <ThemedView type="backgroundElement" style={styles.card}>
          <ThemedText type="smallBold">GET /api/core/v1/users/me</ThemedText>

          {apiUser ? (
            <View style={styles.cardBody}>
              <ThemedText themeColor="textSecondary" type="small">
                id: {apiUser.id}
              </ThemedText>
              <ThemedText themeColor="textSecondary" type="small">
                name: {apiUser.name}
              </ThemedText>
              <ThemedText themeColor="textSecondary" type="small">
                email: {apiUser.email}
              </ThemedText>
              <ThemedText themeColor="textSecondary" type="small">
                emailVerified: {String(apiUser.emailVerified)}
              </ThemedText>
            </View>
          ) : envelope?.error ? (
            <ThemedText style={styles.errorText}>{envelope.error}</ThemedText>
          ) : fetchError ? (
            <ThemedText style={styles.errorText}>{fetchError}</ThemedText>
          ) : (
            <ThemedText themeColor="textSecondary" type="small">
              Loading…
            </ThemedText>
          )}
        </ThemedView>

        <AppButton variant="secondary" label="Call API again" onPress={fetchCurrentUser} disabled={busy} />

        <ThemedView type="backgroundElement" style={styles.card}>
          <ThemedText type="smallBold">Settings</ThemedText>
          <AppTextInput
            label="Name"
            autoComplete="name"
            textContentType="name"
            value={name}
            onChangeText={setName}
            error={fieldErrors.name}
          />
          <AppTextInput
            label="Image URL"
            keyboardType="url"
            autoCapitalize="none"
            autoCorrect={false}
            placeholder="https://…"
            value={image}
            onChangeText={setImage}
            error={fieldErrors.image}
          />
          {settingsMessage ? (
            <ThemedText themeColor="textSecondary" type="small">
              {settingsMessage}
            </ThemedText>
          ) : null}
          <AppButton label="Save settings" loading={saving} onPress={handleSave} disabled={busy || !apiUser} />
        </ThemedView>

        <AppButton label="Sign out" variant="secondary" onPress={() => signOut()} disabled={busy} />
        <AppButton label="Delete account" variant="secondary" loading={deleting} onPress={confirmDelete} disabled={busy} />
      </ScrollView>
    </Screen>
  )
}

const styles = StyleSheet.create({
  content: {
    gap: Spacing.three,
    padding: Spacing.four,
  },
  subtitle: {
    marginBottom: Spacing.two,
  },
  card: {
    borderRadius: 12,
    gap: Spacing.two,
    padding: Spacing.four,
  },
  cardBody: {
    gap: Spacing.one,
  },
  errorText: {
    color: '#e5484d',
    fontSize: 14,
  },
})
