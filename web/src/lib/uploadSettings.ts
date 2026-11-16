import type { LargeFileFormat, UploadSettings } from '../api/models.ts'
import { isUploadSettings } from '../api/models.ts'

function decimalBytes(value: number | string, unit: number): number {
  const scaled = Number(value) * unit
  const bytes = Math.round(scaled)
  // Absorb only floating-point round-trip error, not fractional byte budgets.
  return Number.isFinite(scaled) && Math.abs(scaled - bytes) <= Number.EPSILON * Math.abs(scaled)
    ? bytes
    : NaN
}

// Display units are decimal. The server owns carrier selection and segmentation.
export function uploadSettingsFromDraft(
  largeFileFormat: LargeFileFormat,
  mp4GB: number | string,
  bmpMB: number | string,
): UploadSettings | null {
  const settings: UploadSettings = {
    largeFileFormat,
    mp4PartSize: decimalBytes(mp4GB, 1_000_000_000),
    bmpPartSize: decimalBytes(bmpMB, 1_000_000),
  }
  return isUploadSettings(settings) ? settings : null
}
