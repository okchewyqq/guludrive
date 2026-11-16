import type {
  Account,
  Administrator,
  AdministratorUpdate,
  Bucket,
  DownloadLink,
  DownloadSettingsUpdate,
  Entry,
  GDirectStatus,
  Listing,
  Status,
  SetupStatus,
  Transfer,
  UploadSettings,
} from './models.ts'
import { isDownloadSettings, isUploadSettings } from './models.ts'

const googleStages = ['exchange_master', 'exchange_bearer', 'validate_photos'] as const
const googleReasons = [
  'BadAuthentication',
  'NeedsBrowser',
  'ServiceUnavailable',
  'unknown',
] as const
const remoteOperations = [
  'create_album',
  'upload',
  'commit_upload',
  'add_to_album',
  'trash',
  'download',
] as const
const remoteStages = [
  'request',
  'http_response',
  'read_response',
  'decode_response',
  'decode_album_id',
  'decode_media_id',
  'record_album',
] as const
const remoteReasons = [
  'rejected',
  'network_error',
  'response_too_large',
  'invalid_gzip',
  'unsupported_encoding',
  'invalid_protobuf',
  'missing_album_id',
  'invalid_album_id',
  'missing_media_id',
  'invalid_media_id',
  'journal_failed',
  'unknown',
] as const
type GoogleStage = (typeof googleStages)[number]
type GoogleReason = (typeof googleReasons)[number]
export type RemoteOperation = (typeof remoteOperations)[number]
export type RemoteStage = (typeof remoteStages)[number]
export type RemoteReason = (typeof remoteReasons)[number]
interface Failure {
  code?:
    | 'google_auth_failed'
    | 'remote_operation_failed'
    | 'outcome_uncertain'
    | 'invalid_current_password'
    | 'gdirect_not_ready'
    | 'proxy_origin_required'
    | 'proxy_unsupported_metadata'
    | 'proxy_manifest_too_large'
  operation?: RemoteOperation
  stage?: GoogleStage | RemoteStage
  reason?: GoogleReason | RemoteReason
  upstreamStatus?: number
}

export class ApiError extends Error {
  readonly status: number
  readonly failure: Failure
  constructor(status: number, failure: Failure = {}) {
    super(failure.code ?? 'request_failed')
    this.name = 'ApiError'
    this.status = status
    this.failure = failure
  }
}

// Transport-owned epoch avoids importing reactive session state into the API layer.
let sessionEpoch = 0
export function sessionGeneration(): number {
  return sessionEpoch
}
export function advanceSessionGeneration(): void {
  sessionEpoch++
}
let unauthorizedHandler = () => {}
export function onUnauthorized(handler: () => void): void {
  unauthorizedHandler = handler
}

function knownValue<T extends string>(value: unknown, allowed: readonly T[]): T | undefined {
  return typeof value === 'string' && allowed.includes(value as T) ? (value as T) : undefined
}
function remoteDetails(
  data: Record<string, unknown>,
): Pick<Failure, 'operation' | 'stage' | 'reason'> {
  const details: Pick<Failure, 'operation' | 'stage' | 'reason'> = {
    reason: knownValue(data.reason, remoteReasons) ?? 'unknown',
  }
  const operation = knownValue(data.operation, remoteOperations)
  const stage = knownValue(data.stage, remoteStages)
  if (operation) details.operation = operation
  if (stage) details.stage = stage
  return details
}

// Only explicitly understood diagnostics cross into the UI, never arbitrary backend bodies.
export function responseError(
  status: number,
  body: unknown,
  generation = sessionGeneration(),
): ApiError {
  const data = body && typeof body === 'object' ? (body as Record<string, unknown>) : {}
  const failure: Failure = {}
  if (data.code === 'google_auth_failed') {
    failure.code = data.code
    const remote = remoteDetails(data)
    if (remote.operation) {
      Object.assign(failure, remote)
    } else {
      const stage = knownValue(data.stage, googleStages)
      if (stage) failure.stage = stage
      failure.reason = knownValue(data.reason, googleReasons) ?? 'unknown'
    }
  } else if (
    data.code === 'invalid_current_password' || data.code === 'gdirect_not_ready' ||
    data.code === 'proxy_origin_required' || data.code === 'proxy_unsupported_metadata' ||
    data.code === 'proxy_manifest_too_large'
  ) {
    failure.code = data.code
  } else if (data.code === 'outcome_uncertain' || data.code === 'remote_operation_failed') {
    failure.code = data.code
    Object.assign(failure, remoteDetails(data))
  }
  if (
    failure.code &&
    typeof data.upstreamStatus === 'number' &&
    Number.isInteger(data.upstreamStatus) &&
    data.upstreamStatus >= 100 &&
    data.upstreamStatus <= 599
  )
    failure.upstreamStatus = data.upstreamStatus
  const error = new ApiError(status, failure)
  if (status === 401 && !failure.code && generation === sessionGeneration()) unauthorizedHandler()
  return error
}

export function pathURL(endpoint: '/api/files' | '/api/file', path: string): string {
  return endpoint + '?' + new URLSearchParams({ path }).toString()
}
export function etag(entry: Pick<Entry, 'id' | 'version'>): string {
  return '"' + entry.id + '-' + entry.version + '"'
}

async function request<T>(
  url: string,
  init: RequestInit = {},
  expectBody = true,
): Promise<T> {
  const method = init.method ?? 'GET'
  const headers = new Headers(init.headers)
  if (method !== 'GET' && method !== 'HEAD') headers.set('X-GuluDrive-Request', '1')
  const generation = sessionGeneration()
  const response = await fetch(url, { ...init, method, headers, credentials: 'same-origin' })
  const body: unknown = await response.json().catch(() => null)
  if (!response.ok) throw responseError(response.status, body, generation)
  if (!expectBody) return undefined as T
  if (body === null || typeof body !== 'object') throw new ApiError(502)
  return body as T
}
function json<T>(
  url: string,
  body: unknown,
  headers?: HeadersInit,
  signal?: AbortSignal,
): Promise<T> {
  const h = new Headers(headers)
  h.set('Content-Type', 'application/json')
  return request<T>(url, { method: 'POST', headers: h, body: JSON.stringify(body), signal })
}

async function checkedSettings<T>(
  url: string,
  validate: (value: unknown) => value is T,
  init: RequestInit,
): Promise<T> {
  const value = await request<unknown>(url, init)
  if (!validate(value)) throw new ApiError(502)
  return value
}

export const api = {
  setupStatus: async (signal?: AbortSignal) => {
    const value = await request<SetupStatus>('/api/setup', { signal })
    if (typeof value.initialized !== 'boolean') throw new ApiError(502)
    return value
  },
  setup: (username: string, password: string, signal?: AbortSignal) =>
    json<SetupStatus>('/api/setup', { username, password }, undefined, signal),
  proxyScript: async (publicOrigin: string, password: string, signal?: AbortSignal) => {
    const result = await json<{ script: string }>('/api/proxy-script', { publicOrigin, password }, undefined, signal)
    if (typeof result.script !== 'string') throw new ApiError(502)
    return result
  },
  status: (signal?: AbortSignal) => request<Status>('/api/status', { signal }),
  uploadSettings: (signal?: AbortSignal) =>
    checkedSettings('/api/upload-settings', isUploadSettings, { signal }),
  updateUploadSettings: (settings: UploadSettings, signal?: AbortSignal) =>
    checkedSettings('/api/upload-settings', isUploadSettings, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(settings),
      signal,
    }),
  downloadSettings: (signal?: AbortSignal) =>
    checkedSettings('/api/download-settings', isDownloadSettings, { signal }),
  updateDownloadSettings: (settings: DownloadSettingsUpdate, signal?: AbortSignal) =>
    checkedSettings(
      '/api/download-settings',
      isDownloadSettings,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(settings),
        signal,
      },
    ),
  refreshGDirect: (signal?: AbortSignal) =>
    json<GDirectStatus>('/api/gdirect/refresh', {}, undefined, signal),
  createDownloadLink: (path: string, signal?: AbortSignal) =>
    json<DownloadLink>('/api/download-links', { path }, undefined, signal),
  administrator: (signal?: AbortSignal) => request<Administrator>('/api/admin', { signal }),
  updateAdministrator: (update: AdministratorUpdate, signal?: AbortSignal) =>
    request<Administrator>('/api/admin', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(update),
      signal,
    }),
  login: async (username: string, password: string, signal?: AbortSignal) => {
    const generation = sessionGeneration()
    const result = await json<{ ok: true }>('/api/session', { username, password }, undefined, signal)
    if (signal?.aborted || generation !== sessionGeneration()) return null
    advanceSessionGeneration()
    return result
  },
  logout: () => request<{ ok: true }>('/api/session', { method: 'DELETE' }),
  accounts: () => request<{ accounts: Account[] }>('/api/accounts'),
  connectGoogle: (oauth_token: string) =>
    json<{ account: Account }>('/api/accounts/google', { oauth_token }),
  selectAccount: (id: string) =>
    json<{ ok: true }>('/api/accounts/' + encodeURIComponent(id) + '/select', {}),
  removeAccount: (id: string) =>
    request<{ ok: true }>('/api/accounts/' + encodeURIComponent(id), { method: 'DELETE' }),
  buckets: () => request<{ buckets: Bucket[] }>('/api/buckets'),
  createBucket: (name: string) => json<{ entry: Entry }>('/api/buckets', { name }),
  createDirectory: (path: string) => json<{ entry: Entry }>('/api/directories', { path }),
  list: (path: string, signal?: AbortSignal) =>
    request<Listing>(pathURL('/api/files', path), { signal }),
  remove: (entry: Entry) =>
    request<{ ok: true }>(pathURL('/api/files', entry.path), {
      method: 'DELETE',
      headers: { 'If-Match': etag(entry) },
    }),
  transfer: (operation: 'copy' | 'move', source: Entry, destination: string, overwrite: boolean) =>
    json<{ entry: Entry }>(
      '/api/' + operation,
      {
        source: source.path,
        destination,
        overwrite,
        recursive: source.isDir,
      } satisfies Transfer,
      { 'If-Match': etag(source) },
    ),
}
