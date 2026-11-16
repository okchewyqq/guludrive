import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api, sessionGeneration } from '../api/client.ts'
import type { DownloadMode, DownloadSettings, DownloadSettingsUpdate } from '../api/models'
import { clearClipboardFallback, copyText } from '../lib/clipboard.ts'
import { t, type MessageKey } from '../lib/i18n.ts'
import { validProxyPassword, validProxyURL } from '../lib/proxySettings.ts'
import { noteError, notify } from './feedback.ts'
import { refreshStatus, status } from './session.ts'

// Drafts belong to the view. Only API responses determine the effective mode.
export function useDownloadSettings() {
  const downloadSettings = ref<DownloadSettings | null>(null)
  const draftMode = ref<DownloadMode>('local')
  const publicOrigin = ref('')
  const workerURL = ref('')
  const workerSecret = ref('')
  const downloadLoading = ref(false)
  const downloadSaving = ref(false)
  const scriptCopying = ref(false)
  const ipRefreshing = ref(false)
  const downloadError = ref<unknown>(null)
  const downloadValidation = ref<MessageKey | null>(null)
  const ipSelecting = computed(() =>
    (downloadSaving.value && draftMode.value === 'gdirect') || ipRefreshing.value ||
    !!(status.value?.gdirect.enabled && status.value.gdirect.selecting),
  )
  const downloadBusy = computed(() =>
    downloadLoading.value || downloadSaving.value || scriptCopying.value || ipSelecting.value,
  )
  const proxyHTTP = computed(() => location.protocol === 'http:' ||
    publicOrigin.value.trim().toLowerCase().startsWith('http:') ||
    workerURL.value.trim().toLowerCase().startsWith('http:'),
  )
  let controller: AbortController | null = null
  function apply(settings: DownloadSettings): void {
    downloadSettings.value = settings
    draftMode.value = settings.mode
    publicOrigin.value = settings.publicOrigin
    workerURL.value = settings.workerURL
  }
  function begin() {
    controller?.abort()
    const request = new AbortController()
    const generation = sessionGeneration()
    controller = request
    return { request, current: () => !request.signal.aborted && generation === sessionGeneration() }
  }
  async function loadDownloadSettings(): Promise<void> {
    if (downloadSaving.value || ipRefreshing.value || scriptCopying.value) return
    const { request, current } = begin()
    downloadLoading.value = true
    downloadError.value = null
    downloadValidation.value = null
    try {
      const [settings] = await Promise.all([api.downloadSettings(request.signal), refreshStatus(request.signal)])
      if (current()) apply(settings)
    } catch (failure) {
      if (current()) { downloadError.value = failure; noteError(failure) }
    } finally {
      if (controller === request) downloadLoading.value = false
    }
  }
  function validatePassword(required: boolean): boolean {
    if (required && !workerSecret.value) { downloadValidation.value = 'workerSecretRequired'; return false }
    if (workerSecret.value && !validProxyPassword(workerSecret.value)) {
      downloadValidation.value = 'workerSecretInvalid'
      return false
    }
    return true
  }
  function validateOrigin(): boolean {
    if (validProxyURL(publicOrigin.value.trim(), true)) return true
    downloadValidation.value = 'publicOriginInvalid'
    return false
  }
  async function persist(mode: DownloadMode, configureWorker = false): Promise<void> {
    if (downloadBusy.value || !downloadSettings.value) return
    downloadError.value = null
    downloadValidation.value = null
    if (configureWorker) {
      if (!validateOrigin() || !validatePassword(!downloadSettings.value.workerConfigured)) return
      if (!validProxyURL(workerURL.value.trim())) { downloadValidation.value = 'workerURLInvalid'; return }
    }
    const update: DownloadSettingsUpdate = {
      mode,
      workerURL: configureWorker ? workerURL.value.trim() : downloadSettings.value.workerURL,
      publicOrigin: configureWorker ? publicOrigin.value.trim() : downloadSettings.value.publicOrigin,
      workerSecret: configureWorker ? workerSecret.value : '',
    }
    const { request, current } = begin()
    downloadSaving.value = true
    try {
      const settings = await api.updateDownloadSettings(update, request.signal)
      if (!current()) return
      apply(settings)
      workerSecret.value = ''
      clearClipboardFallback()
      await refreshStatus(request.signal)
      if (current()) notify('settingsSaved')
    } catch (failure) {
      if (current()) { downloadError.value = failure; noteError(failure) }
    } finally {
      delete update.workerSecret
      if (controller === request) downloadSaving.value = false
    }
  }
  async function chooseDownloadMode(mode: DownloadMode): Promise<void> {
    if (downloadBusy.value || !downloadSettings.value) return
    draftMode.value = mode
    downloadError.value = null
    downloadValidation.value = null
    if (mode !== 'proxy' && mode !== downloadSettings.value.mode) await persist(mode)
  }
  async function copyWorkerScript(): Promise<void> {
    if (downloadBusy.value) return
    downloadError.value = null
    downloadValidation.value = null
    if (!validateOrigin() || !validatePassword(true)) return
    clearClipboardFallback()
    const { request, current } = begin()
    scriptCopying.value = true
    let script = ''
    try {
      const result = await api.proxyScript(publicOrigin.value.trim(), workerSecret.value, request.signal)
      script = result.script
      result.script = ''
      if (!current()) return
      const copied = await copyText(script, undefined, { title: t('copyWorkerScript'), close: t('close') })
      if (current()) notify(copied ? 'copied' : 'copyFallback')
      else clearClipboardFallback()
    } catch (failure) {
      if (current()) { downloadError.value = failure; noteError(failure) }
    } finally {
      script = ''
      if (controller === request) scriptCopying.value = false
    }
  }
  async function refreshIP(): Promise<void> {
    if (downloadBusy.value || downloadSettings.value?.mode !== 'gdirect') return
    const { request, current } = begin()
    ipRefreshing.value = true
    downloadError.value = null
    try {
      const result = await api.refreshGDirect(request.signal)
      if (current() && status.value) status.value = { ...status.value, gdirect: result }
    } catch (failure) {
      if (current()) { downloadError.value = failure; noteError(failure) }
    } finally {
      if (controller === request) ipRefreshing.value = false
    }
  }
  onMounted(loadDownloadSettings)
  onBeforeUnmount(() => {
    controller?.abort()
    workerSecret.value = ''
    clearClipboardFallback()
  })
  return {
    downloadSettings, draftMode, publicOrigin, workerURL, workerSecret, proxyHTTP,
    downloadLoading, downloadSaving, scriptCopying, downloadBusy, ipSelecting,
    downloadError, downloadValidation, loadDownloadSettings, chooseDownloadMode, refreshIP,
    copyWorkerScript, saveDownloadSettings: () => persist('proxy', true),
  }
}
