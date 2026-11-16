import type { DownloadLink, Entry } from '../api/models.ts'

export interface DownloadLinkText {
  text: string
  count: number
  expiresAt: string
}

// Keep output in listing order and never return a partial batch after failure.
export async function createDownloadLinkText(
  entries: Entry[],
  create: (path: string, signal?: AbortSignal) => Promise<DownloadLink>,
  signal?: AbortSignal,
): Promise<DownloadLinkText | null> {
  const files = entries.filter((entry) => !entry.isDir)
  if (!files.length) return null
  const lines: string[] = []
  let expiresAt = ''
  for (const entry of files) {
    const link = await create(entry.path, signal)
    lines.push(link.url)
    if (!expiresAt || Date.parse(link.expiresAt) < Date.parse(expiresAt)) expiresAt = link.expiresAt
  }
  return { text: lines.join('\n'), count: lines.length, expiresAt }
}
