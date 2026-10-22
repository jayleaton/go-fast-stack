import path from 'node:path'
import { defineConfig } from 'vitest/config'

// Unit tests: pure TypeScript under src/ (no React Native runtime).
export default defineConfig({
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, 'src') },
  },
})
