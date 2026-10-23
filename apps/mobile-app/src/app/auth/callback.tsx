import { Redirect } from 'expo-router'

/**
 * The deep link Google/Apple sign-in returns to (AppRoutes.authCallback).
 * openAuthSessionAsync in socialSignIn.ts reads the code from it; when the
 * OS also opens the link in the app (Android), this sends the user on
 * instead of showing "Unmatched route". The one-time code in the query is
 * dropped here, never read.
 */
export default function AuthCallback() {
  return <Redirect href="/" />
}
