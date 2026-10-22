// Just enough of react-native for @better-auth/expo/client in Node.
export const Platform = { OS: 'ios' as const }
export const AppState = {
  addEventListener: () => ({ remove: () => {} }),
}
