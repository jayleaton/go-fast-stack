// In-memory keychain. It lives on globalThis so it survives vi.resetModules(),
// which is how the contract tests simulate an app relaunch.
const key = Symbol.for('contract.secureStore')
const g = globalThis as Record<symbol, Map<string, string> | undefined>
const store = (g[key] ??= new Map<string, string>())

export function getItem(name: string): string | null {
  return store.get(name) ?? null
}

export function setItem(name: string, value: string): void {
  store.set(name, value)
}

export async function getItemAsync(name: string): Promise<string | null> {
  return getItem(name)
}

export async function setItemAsync(name: string, value: string): Promise<void> {
  store.set(name, value)
}

export async function deleteItemAsync(name: string): Promise<void> {
  store.delete(name)
}

/** Test helper: wipes the keychain (a fresh install). */
export function clearAll(): void {
  store.clear()
}
