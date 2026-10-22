// A standalone build's origin: `<scheme>://` plus the path.
export function createURL(path: string, options?: { scheme?: string }): string {
  return `${options?.scheme ?? 'betterstack'}://${path.replace(/^\//, '')}`
}
