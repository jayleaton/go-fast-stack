export async function openAuthSessionAsync(): Promise<{ type: 'cancel' }> {
  return { type: 'cancel' }
}
export function dismissAuthSession() {}
