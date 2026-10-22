import path from 'node:path'
import { defineConfig } from 'vitest/config'

const stub = (name: string) => path.resolve(import.meta.dirname, 'contract/stubs', name)

// Contract tests: the app's real auth client and API client against a real
// Go server and Postgres (see contract/README.md). The native modules those
// clients import are replaced with small Node stubs.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['contract/**/*.test.ts'],
    globalSetup: ['contract/globalSetup.ts'],
    setupFiles: ['contract/setup.ts'],
    testTimeout: 30_000,
    hookTimeout: 30_000,
    // A forked worker is left with open handles after these tests and times
    // out on exit; worker threads terminate cleanly.
    pool: 'threads',
    // Inline the Expo auth plugin so the aliases below apply to its imports.
    server: { deps: { inline: ['@better-auth/expo'] } },
  },
  resolve: {
    alias: [
      { find: /^@\//, replacement: path.resolve(import.meta.dirname, 'src') + '/' },
      { find: /^react-native$/, replacement: stub('react-native.ts') },
      { find: /^expo-constants$/, replacement: stub('expo-constants.ts') },
      { find: /^expo-linking$/, replacement: stub('expo-linking.ts') },
      { find: /^expo-network$/, replacement: stub('expo-network.ts') },
      { find: /^expo-secure-store$/, replacement: stub('expo-secure-store.ts') },
      { find: /^expo-web-browser$/, replacement: stub('expo-web-browser.ts') },
    ],
  },
})
