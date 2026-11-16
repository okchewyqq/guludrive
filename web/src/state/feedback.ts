import { ref } from 'vue'
import { ApiError, type RemoteOperation, type RemoteReason, type RemoteStage } from '../api/client.ts'
import { t, type MessageKey } from '../lib/i18n.ts'

export function describeError(error: unknown): string {
  if (error instanceof DOMException && error.name === 'AbortError') return t('cancelled')
  if (error instanceof TypeError) return t('networkError')
  if (!(error instanceof ApiError)) return t('requestFailed')
  const upload = error.failure.operation === 'upload' || error.failure.operation === 'commit_upload'
  if (error.failure.code === 'outcome_uncertain')
    return t(upload ? 'uploadUncertain' : 'uncertain')
  if (error.failure.code === 'remote_operation_failed' && upload) return t('uploadFailed')
  if (
    error.failure.code === 'remote_operation_failed' ||
    (error.failure.code === 'google_auth_failed' && error.failure.operation)
  ) {
    const operations: Record<RemoteOperation, MessageKey> = {
      create_album: 'operationCreateAlbum',
      upload: 'operationUpload',
      commit_upload: 'operationCommitUpload',
      add_to_album: 'operationAddToAlbum',
      trash: 'operationTrash',
      download: 'operationDownload',
    }
    const stages: Record<RemoteStage, MessageKey> = {
      request: 'stageRequest',
      http_response: 'stageHTTP',
      read_response: 'stageRead',
      decode_response: 'stageDecode',
      decode_album_id: 'stageAlbumID',
      decode_media_id: 'stageMediaID',
      record_album: 'stageRecordAlbum',
    }
    const reasons: Record<RemoteReason, MessageKey> = {
      rejected: 'remoteRejected',
      network_error: 'remoteNetwork',
      response_too_large: 'remoteTooLarge',
      invalid_gzip: 'remoteGzip',
      unsupported_encoding: 'remoteEncoding',
      invalid_protobuf: 'remoteProtobuf',
      missing_album_id: 'remoteAlbumID',
      invalid_album_id: 'remoteInvalidAlbumID',
      missing_media_id: 'remoteMediaID',
      invalid_media_id: 'remoteInvalidMediaID',
      journal_failed: 'remoteJournal',
      unknown: 'remoteUnknown',
    }
    const stageKey = error.failure.stage ? stages[error.failure.stage as RemoteStage] : undefined
    const reasonKey = reasons[error.failure.reason as RemoteReason] ?? 'remoteUnknown'
    const details = [
      error.failure.operation ? t(operations[error.failure.operation]) : '',
      stageKey ? t(stageKey) : '',
      error.failure.upstreamStatus ? t('upstream', { status: error.failure.upstreamStatus }) : '',
      t(reasonKey),
    ].filter(Boolean)
    const title = error.failure.code === 'google_auth_failed' ? 'googleAuthFailed' : 'remoteFailed'
    return t(title) + ' · ' + details.join(' / ')
  }
  if (error.failure.code === 'invalid_current_password') return t('invalidCurrentPassword')
  if (error.failure.code === 'gdirect_not_ready') return t('gdirectNotReady')
  if (error.failure.code === 'proxy_origin_required') return t('proxyOriginRequired')
  if (error.failure.code === 'proxy_unsupported_metadata') return t('proxyUnsupportedMetadata')
  if (error.failure.code === 'proxy_manifest_too_large') return t('proxyManifestTooLarge')
  if (error.failure.code === 'google_auth_failed') {
    const reason: Record<string, MessageKey> = {
      BadAuthentication: 'reasonBadAuthentication',
      NeedsBrowser: 'reasonNeedsBrowser',
      ServiceUnavailable: 'reasonServiceUnavailable',
      unknown: 'reasonUnknown',
    }
    const stages: Record<string, MessageKey> = {
      exchange_master: 'stageExchangeMaster',
      exchange_bearer: 'stageExchangeBearer',
      validate_photos: 'stageValidatePhotos',
    }
    const details = [
      error.failure.stage ? t(stages[error.failure.stage]!) : '',
      error.failure.upstreamStatus ? t('upstream', { status: error.failure.upstreamStatus }) : '',
    ].filter(Boolean)
    return (
      t('googleAuthFailed') +
      (details.length ? ' · ' + details.join(' / ') : '') +
      ' — ' +
      t(reason[error.failure.reason ?? 'unknown']!)
    )
  }
  const statuses: Record<number, MessageKey> = {
    400: 'invalidRequest',
    401: 'sessionExpired',
    403: 'forbidden',
    404: 'notFound',
    408: 'timeout',
    409: 'conflict',
    412: 'precondition',
    413: 'tooLarge',
    415: 'invalidRequest',
    422: 'invalidRequest',
    501: 'unsupported',
  }
  return t(statuses[error.status] ?? 'requestFailed')
}
export const uncertainOutcome = ref(false)
export function noteError(error: unknown): void {
  if (error instanceof ApiError && error.failure.code === 'outcome_uncertain')
    uncertainOutcome.value = true
}
export const notices = ref<Array<{
  id: number
  message?: MessageKey
  values?: Record<string, string | number>
  error?: unknown
}>>([])
let nextID = 0
export function dismissNotice(id: number): void {
  notices.value = notices.value.filter((notice) => notice.id !== id)
}
export function notify(message: MessageKey, values?: Record<string, string | number>): void {
  const id = ++nextID
  notices.value = [...notices.value.slice(-2), { id, message, values }]
  window.setTimeout(() => dismissNotice(id), 4500)
}
export function reportError(error: unknown): void {
  noteError(error)
  const id = ++nextID
  notices.value = [...notices.value.slice(-2), { id, error }]
}
