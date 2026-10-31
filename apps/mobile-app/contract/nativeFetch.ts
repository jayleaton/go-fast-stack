import http from 'node:http'
import https from 'node:https'

/**
 * fetch as the native app sends it. React Native's fetch (NSURLSession on
 * iOS, OkHttp on Android) sends only the headers the caller set. Node's fetch
 * (undici) adds the browser's Fetch Metadata (`Sec-Fetch-Mode: cors`), which
 * the Go server's API gate (apps/web/internal/server/middleware.go, apiOnly)
 * rightly treats as a browser and answers with 404. This sends the request
 * over node:http instead, so the contract test exercises the API as the app
 * reaches it.
 *
 * It covers what the app's clients use: method, headers, a buffered body,
 * an AbortSignal and `redirect` (follow by default, or manual).
 */
const maxRedirects = 20
const redirectStatuses = new Set([301, 302, 303, 307, 308])
const nullBodyStatuses = new Set([101, 204, 205, 304])

export async function nativeFetch(input: string | URL | Request, init?: RequestInit): Promise<Response> {
  const request = new Request(input, init)
  const body = request.body ? Buffer.from(await request.arrayBuffer()) : undefined
  let url = new URL(request.url)
  let method = request.method
  let payload = body
  for (let hops = 0; ; hops++) {
    const response = await send(url, method, request.headers, payload, request.signal)
    const location = response.headers.get('location')
    if (request.redirect === 'manual' || !redirectStatuses.has(response.status) || !location) return response
    if (request.redirect === 'error') throw new TypeError(`fetch: redirect to ${location}`)
    if (hops >= maxRedirects) throw new TypeError('fetch: too many redirects')
    url = new URL(location, url)
    // As fetch does: a 303, or a 301/302 after POST, becomes a bodiless GET.
    if (response.status === 303 || ((response.status === 301 || response.status === 302) && method === 'POST')) {
      method = 'GET'
      payload = undefined
    }
  }
}

function send(url: URL, method: string, headers: Headers, body: Buffer | undefined, signal: AbortSignal): Promise<Response> {
  const outgoing: Record<string, string> = {}
  headers.forEach((value, name) => (outgoing[name] = value))
  if (body) outgoing['content-length'] = String(body.length)
  const transport = url.protocol === 'https:' ? https : http
  return new Promise((resolve, reject) => {
    const req = transport.request(url, { method, headers: outgoing, signal }, res => {
      const chunks: Buffer[] = []
      res.on('data', (chunk: Buffer) => chunks.push(chunk))
      res.on('error', reject)
      res.on('end', () => {
        const responseHeaders = new Headers()
        for (let index = 0; index < res.rawHeaders.length; index += 2) {
          responseHeaders.append(res.rawHeaders[index], res.rawHeaders[index + 1])
        }
        const status = res.statusCode ?? 0
        const response = new Response(nullBodyStatuses.has(status) ? null : Buffer.concat(chunks), {
          status,
          statusText: res.statusMessage,
          headers: responseHeaders,
        })
        Object.defineProperty(response, 'url', { value: url.toString() })
        resolve(response)
      })
    })
    req.on('error', reject)
    req.end(body)
  })
}
