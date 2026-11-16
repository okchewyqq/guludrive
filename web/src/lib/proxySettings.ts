// Public-host reachability is validated by the server; custom HTTP(S) ports are allowed.
export function validProxyURL(value: string, originOnly = false): boolean {
  try {
    const url = new URL(value)
    const authority = value.trim().match(/^https?:\/\/([^/?#]*)/i)?.[1]
    return ['http:', 'https:'].includes(url.protocol) && authority !== undefined && !authority.includes('@') &&
      !url.username && !url.password && !value.includes('?') && !value.includes('#') &&
      (!originOnly || url.pathname === '/')
  } catch { return false }
}

// Preserve raw UTF-8, including spaces; reject controls and unpaired JS surrogates.
export function validProxyPassword(value: string): boolean {
  if (!value || /[\0\r\n]/.test(value)) return false
  const encoded = new TextEncoder().encode(value)
  return encoded.byteLength <= 4096 && new TextDecoder('utf-8', { ignoreBOM: true }).decode(encoded) === value
}
