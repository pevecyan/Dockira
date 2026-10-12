// Small fetch wrapper, request<T>(): JSON in and out, errors as
// ApiError with the server's message, and a 401 {login: true} switches the app to the login form.

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public body?: any,
  ) {
    super(message)
  }
}

let loginHandler: (() => void) | null = null
export function onLoginRequired(fn: () => void) {
  loginHandler = fn
}

let confirmHandler: (() => Promise<boolean>) | null = null
// Called on 403 {confirm: true}: ask for the password again, then the request is retried once.
export function onConfirmRequired(fn: () => Promise<boolean>) {
  confirmHandler = fn
}

export interface RequestOptions {
  headers?: Record<string, string>
  signal?: AbortSignal
}

export async function request<T>(method: string, path: string, body?: unknown, opts: RequestOptions = {}, retried = false): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json', ...opts.headers }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: 'same-origin',
    signal: opts.signal,
  })
  if (res.ok) {
    if (res.status === 204) return undefined as T
    const ct = res.headers.get('Content-Type') ?? ''
    return (ct.includes('json') ? res.json() : res.text()) as Promise<T>
  }
  const data = await res.json().catch(() => null)
  if (res.status === 401 && data?.login) loginHandler?.()
  if (res.status === 403 && data?.confirm && !retried && confirmHandler) {
    if (await confirmHandler()) return request<T>(method, path, body, opts, true)
  }
  throw new ApiError(res.status, errorMessage(data, res), data)
}

export function errorMessage(data: any, res?: Response): string {
  if (data) {
    const details = Array.isArray(data.errors) ? data.errors.map((e: any) => e.message).filter(Boolean) : []
    const main = data.detail || data.title || data.error
    if (main) return details.length ? `${main}: ${details.join('; ')}` : main
  }
  return res ? `${res.status} ${res.statusText}` : 'request failed'
}

export const get = <T>(path: string, opts?: RequestOptions) => request<T>('GET', path, undefined, opts)
export const post = <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('POST', path, body ?? {}, opts)
export const put = <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('PUT', path, body ?? {}, opts)
export const del = <T>(path: string, opts?: RequestOptions) => request<T>('DELETE', path, undefined, opts)

export function qs(params: Record<string, string | number | boolean | undefined | null>): string {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== '') p.set(k, String(v))
  const s = p.toString()
  return s ? `?${s}` : ''
}
