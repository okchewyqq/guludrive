import { etag, pathURL, responseError, sessionGeneration } from './client.ts'
import type { Entry } from './models.ts'

export function uploadFile(
  file: File,
  path: string,
  existing: Entry | undefined,
  progress: (loaded: number, total: number) => void,
  signal: AbortSignal,
): Promise<Entry> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    const abort = () => xhr.abort()
    const clean = () => signal.removeEventListener('abort', abort)
    xhr.open('PUT', pathURL('/api/files', path))
    xhr.withCredentials = true
    xhr.setRequestHeader('X-GuluDrive-Request', '1')
    xhr.setRequestHeader('Content-Type', file.type || 'application/octet-stream')
    xhr.setRequestHeader(existing ? 'If-Match' : 'If-None-Match', existing ? etag(existing) : '*')
    // File is sent directly; the browser supplies its known Content-Length.
    xhr.upload.onprogress = (event) =>
      progress(event.loaded, event.lengthComputable ? event.total : file.size)
    xhr.onload = () => {
      clean()
      let body: unknown = null
      try {
        body = JSON.parse(xhr.responseText)
      } catch {
        /* Ignore arbitrary/non-JSON error bodies. */
      }
      if (xhr.status < 200 || xhr.status >= 300) reject(responseError(xhr.status, body, generation))
      else if (body && typeof body === 'object' && 'entry' in body)
        resolve((body as { entry: Entry }).entry)
      else reject(new Error('invalid_response'))
    }
    xhr.onerror = () => {
      clean()
      reject(new TypeError('network_error'))
    }
    xhr.onabort = () => {
      clean()
      reject(new DOMException('Upload cancelled', 'AbortError'))
    }
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) {
      clean()
      reject(new DOMException('Upload cancelled', 'AbortError'))
      return
    }
    const generation = sessionGeneration()
    xhr.send(file)
  })
}
