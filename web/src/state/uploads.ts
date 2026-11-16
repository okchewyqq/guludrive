import { computed, ref } from 'vue'
import { ApiError } from '../api/client'
import { uploadFile } from '../api/upload'
import type { Entry } from '../api/models'
import { joinPath } from '../lib/paths'
import { noteError, reportError } from './feedback'
import { refreshBuckets } from './workspace'

type UploadState =
  | 'queued'
  | 'uploading'
  | 'publishing'
  | 'uploaded'
  | 'failed'
  | 'cancelled'
  | 'skipped'
interface UploadItem {
  id: number
  name: string
  path: string
  size: number
  percent: number
  state: UploadState
  error?: unknown
}
interface UploadCandidate {
  file: File
  existing?: Entry
}
export const uploadItems = ref<UploadItem[]>([])
export const uploadRevision = ref(0)
export const uploadsRunning = ref(false)
export const uploadCompleted = computed(
  () => uploadItems.value.filter((item) => item.state === 'uploaded').length,
)
let nextID = 0
let activeID: number | null = null
let controller: AbortController | null = null
const files = new Map<number, UploadCandidate>()

export function cancelUpload(id: number): void {
  const item = uploadItems.value.find((value) => value.id === id)
  if (!item) return
  if (id === activeID) controller?.abort()
  else if (item.state === 'queued') {
    item.state = 'skipped'
    files.delete(id)
  }
}
export function clearFinishedUploads(): void {
  uploadItems.value = uploadItems.value.filter((item) =>
    ['queued', 'uploading', 'publishing'].includes(item.state),
  )
}
export function enqueueUploads(candidates: UploadCandidate[], directory: string): void {
  if (uploadsRunning.value || candidates.length === 0) return
  clearFinishedUploads()
  uploadItems.value = candidates.map(({ file, existing }) => {
    const id = ++nextID
    files.set(id, { file, existing })
    return {
      id,
      name: file.name,
      path: joinPath(directory, file.name),
      size: file.size,
      percent: 0,
      state: 'queued',
    }
  })
  void runUploads()
}
async function runUploads(): Promise<void> {
  uploadsRunning.value = true
  let stop = false
  for (const item of uploadItems.value) {
    if (item.state !== 'queued') continue
    if (stop) {
      item.state = 'skipped'
      files.delete(item.id)
      continue
    }
    const candidate = files.get(item.id)
    if (!candidate) continue
    activeID = item.id
    controller = new AbortController()
    item.state = 'uploading'
    try {
      await uploadFile(
        candidate.file,
        item.path,
        candidate.existing,
        (loaded, total) => {
          item.percent = total ? Math.min(100, Math.round((loaded / total) * 100)) : 0
          if (loaded >= total) item.state = 'publishing'
        },
        controller.signal,
      )
      item.percent = 100
      item.state = 'uploaded'
    } catch (error) {
      item.error = error
      item.state =
        error instanceof DOMException && error.name === 'AbortError' ? 'cancelled' : 'failed'
      noteError(error)
      stop =
        item.state === 'cancelled' ||
        (error instanceof ApiError && (error.status === 401 || !!error.failure.code))
    } finally {
      files.delete(item.id)
      controller = null
      activeID = null
      uploadRevision.value++
    }
  }
  uploadsRunning.value = false
  try {
    await refreshBuckets()
  } catch (error) {
    reportError(error)
  }
}
