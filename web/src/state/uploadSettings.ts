import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api, sessionGeneration } from '../api/client'
import { defaultUploadSettings, type LargeFileFormat, type UploadSettings } from '../api/models'
import { uploadSettingsFromDraft } from '../lib/uploadSettings'
import { noteError, notify } from './feedback'

export function useUploadSettings() {
  const uploadSettings = ref<UploadSettings | null>(null)
  const largeFileFormat = ref<LargeFileFormat>(defaultUploadSettings.largeFileFormat)
  const mp4GB = ref<number | string>(defaultUploadSettings.mp4PartSize / 1_000_000_000)
  const bmpMB = ref<number | string>(defaultUploadSettings.bmpPartSize / 1_000_000)
  const uploadLoading = ref(false)
  const uploadSaving = ref(false)
  const uploadError = ref<unknown>(null)
  const uploadValidation = ref(false)
  const uploadBusy = computed(() => uploadLoading.value || uploadSaving.value)
  const draft = computed(() => uploadSettingsFromDraft(largeFileFormat.value, mp4GB.value, bmpMB.value))
  const uploadChanged = computed(() => {
    const saved = uploadSettings.value
    const next = draft.value
    return !!saved && (!next || next.largeFileFormat !== saved.largeFileFormat ||
      next.mp4PartSize !== saved.mp4PartSize || next.bmpPartSize !== saved.bmpPartSize)
  })
  let controller: AbortController | null = null
  function begin() {
    controller?.abort()
    const request = new AbortController()
    const generation = sessionGeneration()
    controller = request
    return {
      request,
      current: () => controller === request && !request.signal.aborted && generation === sessionGeneration(),
    }
  }
  function apply(settings: UploadSettings, resetDraft: boolean): void {
    uploadSettings.value = settings
    if (!resetDraft) return
    largeFileFormat.value = settings.largeFileFormat
    mp4GB.value = settings.mp4PartSize / 1_000_000_000
    bmpMB.value = settings.bmpPartSize / 1_000_000
  }
  async function loadUploadSettings(): Promise<void> {
    if (uploadSaving.value) return
    const { request, current } = begin()
    uploadLoading.value = true
    uploadError.value = null
    uploadValidation.value = false
    try {
      const settings = await api.uploadSettings(request.signal)
      if (current()) apply(settings, uploadSettings.value === null)
    } catch (failure) {
      if (current()) {
        uploadError.value = failure
        noteError(failure)
      }
    } finally {
      if (controller === request) uploadLoading.value = false
    }
  }
  async function saveUploadSettings(): Promise<void> {
    if (uploadBusy.value || !uploadSettings.value) return
    uploadError.value = null
    uploadValidation.value = !draft.value
    if (!draft.value) return
    const update = { ...draft.value }
    const { request, current } = begin()
    uploadSaving.value = true
    try {
      const settings = await api.updateUploadSettings(update, request.signal)
      if (current()) {
        apply(settings, true)
        notify('settingsSaved')
      }
    } catch (failure) {
      if (current()) {
        uploadError.value = failure
        noteError(failure)
      }
    } finally {
      if (controller === request) uploadSaving.value = false
    }
  }
  onMounted(loadUploadSettings)
  onBeforeUnmount(() => controller?.abort())
  return {
    uploadSettings, largeFileFormat, mp4GB, bmpMB, uploadLoading, uploadSaving,
    uploadBusy, uploadChanged, uploadError, uploadValidation, loadUploadSettings, saveUploadSettings,
  }
}
