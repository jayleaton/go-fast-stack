import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { createWriteStream, mkdirSync } from 'node:fs'
import { createServer } from 'node:net'
import path from 'node:path'

import type { TestProject } from 'vitest/node'

/**
 * Starts the real Go server for the contract tests:
 *
 * 1. Builds apps/web/cmd/server from GO_SERVER_SRC (default: this repo), or
 *    uses CONTRACT_SERVER_BIN as is.
 * 2. Uses CONTRACT_DATABASE_URL, or starts a throwaway Postgres container
 *    (tmpfs, free port, removed on teardown).
 * 3. Runs `server migrate`, then `server serve` on a free port in development
 *    mode (email verification off, links logged by the dev mailer).
 *
 * Server output goes to .contract/server.log.
 */

const appDir = path.resolve(import.meta.dirname, '..')
const workDir = path.join(appDir, '.contract')
const deadline = (ms: number) => Date.now() + ms

declare module 'vitest' {
  export interface ProvidedContext {
    apiUrl: string
  }
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer()
    srv.once('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address() as { port: number }
      srv.close(() => resolve(port))
    })
  })
}

async function waitFor(what: string, ms: number, ready: () => Promise<boolean> | boolean) {
  const until = deadline(ms)
  while (Date.now() < until) {
    if (await ready()) return
    await new Promise(r => setTimeout(r, 200))
  }
  throw new Error(`contract setup: timed out waiting for ${what}`)
}

function buildServer(): string {
  if (process.env.CONTRACT_SERVER_BIN) return path.resolve(process.env.CONTRACT_SERVER_BIN)
  const src = path.resolve(process.env.GO_SERVER_SRC ?? path.join(appDir, '../..'))
  const bin = path.join(workDir, 'server')
  // Build scratch beside the output, not in a (possibly RAM-backed) /tmp.
  const goTmp = path.join(workDir, 'gotmp')
  mkdirSync(goTmp, { recursive: true })
  execFileSync(process.env.GO ?? 'go', ['build', '-o', bin, './apps/web/cmd/server'], {
    cwd: src,
    stdio: 'inherit',
    env: { ...process.env, GOTMPDIR: process.env.GOTMPDIR ?? goTmp },
  })
  return bin
}

async function startPostgres(): Promise<{ url: string; stop: () => void }> {
  if (process.env.CONTRACT_DATABASE_URL) return { url: process.env.CONTRACT_DATABASE_URL, stop: () => {} }
  const port = await freePort()
  const name = `mobile-contract-${process.pid}`
  execFileSync('docker', [
    'run', '-d', '--rm', '--name', name,
    '-e', 'POSTGRES_PASSWORD=postgres',
    '-p', `127.0.0.1:${port}:5432`,
    '--tmpfs', '/var/lib/postgresql',
    'postgres:18-alpine',
  ], { stdio: 'ignore' })
  const stop = () => execFileSync('docker', ['rm', '-f', name], { stdio: 'ignore' })
  try {
    // TCP only accepts connections once the init scripts are done.
    await waitFor('Postgres', 60_000, () => {
      try {
        execFileSync('docker', ['exec', name, 'psql', '-h', '127.0.0.1', '-U', 'postgres', '-c', 'select 1'], { stdio: 'ignore' })
        return true
      } catch {
        return false
      }
    })
  } catch (err) {
    stop()
    throw err
  }
  return { url: `postgres://postgres:postgres@127.0.0.1:${port}/postgres?sslmode=disable`, stop }
}

export default async function setup(project: TestProject) {
  mkdirSync(workDir, { recursive: true })
  const bin = buildServer()
  const db = await startPostgres()
  let server: ChildProcess | undefined

  const teardown = () => {
    server?.kill('SIGTERM')
    db.stop()
  }

  try {
    const port = await freePort()
    const apiUrl = `http://127.0.0.1:${port}`
    const env = {
      ...process.env,
      APP_ENV: 'development',
      DATABASE_URL: db.url,
      AUTH_SECRET: randomBytes(32).toString('hex'),
      PUBLIC_URL: apiUrl,
      PORT: String(port),
      RESEND_API_KEY: '',
    }
    execFileSync(bin, ['migrate'], { env, stdio: 'inherit' })

    const log = createWriteStream(path.join(workDir, 'server.log'))
    server = spawn(bin, ['serve'], { env, stdio: ['ignore', 'pipe', 'pipe'] })
    server.stdout?.pipe(log)
    server.stderr?.pipe(log)
    let exited = false
    server.once('exit', () => (exited = true))

    await waitFor('the Go server /healthz', 30_000, async () => {
      if (exited) throw new Error('contract setup: server exited, see .contract/server.log')
      try {
        return (await fetch(`${apiUrl}/healthz`)).ok
      } catch {
        return false
      }
    })

    project.provide('apiUrl', apiUrl)
  } catch (err) {
    teardown()
    throw err
  }

  return teardown
}
