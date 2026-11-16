// Public JSON contracts of the same-origin Go API. No credentials are persisted.
export interface SetupStatus {
  initialized: boolean
}
export interface Administrator {
  username: string
}
export interface AdministratorUpdate {
  currentPassword: string
  username?: string
  newPassword?: string
}
export interface GDirectStatus {
  enabled: boolean
  ip: string
  selecting: boolean
}
export interface Status {
  version: string
  gdirect: GDirectStatus
}
export type DownloadMode = 'local' | 'gdirect' | 'proxy'
export interface DownloadSettings {
  mode: DownloadMode
  workerURL: string
  workerConfigured: boolean
  publicOrigin: string
}
export interface DownloadSettingsUpdate {
  mode: DownloadMode
  workerURL: string
  workerSecret?: string
  publicOrigin: string
}
export type LargeFileFormat = 'mp4' | 'bmp'
export interface UploadSettings {
  largeFileFormat: LargeFileFormat
  // Decimal bytes, including the carrier overhead; not a logical file-size limit.
  mp4PartSize: number
  bmpPartSize: number
}
export const defaultUploadSettings: Readonly<UploadSettings> = Object.freeze({
  largeFileFormat: 'bmp',
  mp4PartSize: 6_000_000_000,
  bmpPartSize: 150_000_000,
})
export function isUploadSettings(value: unknown): value is UploadSettings {
  if (!value || typeof value !== 'object') return false
  const settings = value as UploadSettings
  return (settings.largeFileFormat === 'mp4' || settings.largeFileFormat === 'bmp') &&
    Number.isSafeInteger(settings.mp4PartSize) &&
    settings.mp4PartSize >= 1_000_000_000 && settings.mp4PartSize <= 9_000_000_000 &&
    Number.isSafeInteger(settings.bmpPartSize) &&
    settings.bmpPartSize >= 100_000_000 && settings.bmpPartSize <= 150_000_000
}
export function isDownloadSettings(value: unknown): value is DownloadSettings {
  if (!value || typeof value !== 'object') return false
  const settings = value as DownloadSettings
  return ['local', 'gdirect', 'proxy'].includes(settings.mode) &&
    typeof settings.workerURL === 'string' &&
    typeof settings.workerConfigured === 'boolean' &&
    typeof settings.publicOrigin === 'string'
}
export interface DownloadLink {
  url: string
  expiresAt: string
}
export interface Account {
  id: string
  email: string
  selected: boolean
}
export interface Bucket {
  id: number
  name: string
  album: { id: string; accountID: string }
}
export interface Entry {
  id: number
  path: string
  name: string
  isDir: boolean
  size: number
  modTime: string
  version: number
  contentType: string
  bucket: string
}
export interface Listing {
  entry: Entry
  entries: Entry[]
}
export interface Transfer {
  source: string
  destination: string
  overwrite: boolean
  recursive: boolean
}
