import { readFileSync } from 'node:fs'
import path from 'node:path'

import { describe, expect, it } from 'vitest'

import { ApiRoutes, userPath } from './routes'

// The Go server's route table, read from this checkout. Catches API path
// drift without building or running the server (the contract test checks
// behaviour; this checks that every path the app calls still exists).
const goRoutesFile = path.resolve(import.meta.dirname, '../../../../web/internal/config/routes.go')

function goApiPaths(): Set<string> {
  const src = readFileSync(goRoutesFile, 'utf8')
  return new Set([...src.matchAll(/"(\/api\/core\/[^"]+)"/g)].map(m => m[1]))
}

function appPaths(node: unknown): string[] {
  if (typeof node === 'string') return [node]
  return Object.values(node as Record<string, unknown>).flatMap(appPaths)
}

describe('ApiRoutes', () => {
  it('only uses paths the Go server routes (apps/web/internal/config/routes.go)', () => {
    const goPaths = goApiPaths()
    expect(goPaths.size).toBeGreaterThan(0)
    const missing = appPaths(ApiRoutes).filter(p => !goPaths.has(p))
    expect(missing).toEqual([])
  })

  it('builds concrete paths with an escaped id', () => {
    expect(userPath('a/b')).toBe('/api/core/v1/users/a%2Fb')
  })
})
