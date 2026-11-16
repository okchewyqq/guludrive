<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Globe, Link2, LogOut, Moon, RefreshCw, Server, UserRound } from '@lucide/vue'
import BaseDialog from '../components/BaseDialog.vue'
import CopyField from '../components/CopyField.vue'
import ErrorBanner from '../components/ErrorBanner.vue'
import UploadSettingsCard from '../components/UploadSettingsCard.vue'
import { t, type MessageKey } from '../lib/i18n'
import { describeError, noteError, notify } from '../state/feedback'
import { language, theme, type Language, type Theme } from '../state/preferences'
import { clearSession, signOut, status } from '../state/session'
import { uploadsRunning } from '../state/uploads'
import { api, ApiError, sessionGeneration } from '../api/client'
import type { AdministratorUpdate, DownloadMode } from '../api/models'
import { useDownloadSettings } from '../state/downloadSettings'

const serverAddress = location.origin
const davAddress = new URL('/dav/', location.origin).href
const logoutOpen = ref(false)
const busy = ref(false)
const error = ref<unknown>(null)
const blocked = ref(false)
const {
  downloadSettings, draftMode, publicOrigin, workerURL, workerSecret, proxyHTTP,
  downloadLoading, downloadSaving, scriptCopying, copyWorkerScript,
  downloadBusy, ipSelecting, downloadError, downloadValidation, loadDownloadSettings,
  chooseDownloadMode, refreshIP, saveDownloadSettings,
} = useDownloadSettings()
const downloadModes: Array<{ value: DownloadMode; label: MessageKey; help: MessageKey }> = [
  { value: 'local', label: 'downloadLocal', help: 'downloadLocalHelp' },
  { value: 'gdirect', label: 'gdirect', help: 'gdirectHelp' },
  { value: 'proxy', label: 'downloadProxy', help: 'downloadProxyHelp' },
]
function modeLabel(mode: DownloadMode): MessageKey {
  return mode === 'local' ? 'downloadLocal' : mode === 'gdirect' ? 'gdirect' : 'downloadProxy'
}
const languageOptions: Array<{ value: Language; label: string }> = [
  { value: 'system', label: t('systemLabel') },
  { value: 'zh', label: '中文' },
  { value: 'en', label: 'English' },
]
const themeOptions: Array<{ value: Theme; label: string }> = [
  { value: 'system', label: t('systemLabel') },
  { value: 'light', label: t('themeLight') },
  { value: 'dark', label: t('themeDark') },
]
const adminUsername = ref('')
const savedUsername = ref<string | null>(null)
const currentPassword = ref('')
const newPassword = ref('')
const confirmPassword = ref('')
const adminLoading = ref(false)
const adminSaving = ref(false)
const adminError = ref<unknown>(null)
const adminValidation = ref<MessageKey | null>(null)
const adminChanged = computed(() => adminUsername.value !== savedUsername.value || !!newPassword.value)
let adminController: AbortController | null = null
function clearAdminPasswords(): void {
  currentPassword.value = ''
  newPassword.value = ''
  confirmPassword.value = ''
}
async function loadAdministrator(): Promise<void> {
  if (adminSaving.value) return
  adminController?.abort()
  const request = new AbortController()
  const generation = sessionGeneration()
  adminController = request
  adminLoading.value = true
  adminError.value = null
  adminValidation.value = null
  clearAdminPasswords()
  try {
    const admin = await api.administrator(request.signal)
    if (!request.signal.aborted && generation === sessionGeneration()) {
      savedUsername.value = admin.username
      adminUsername.value = admin.username
    }
  } catch (failure) {
    if (!request.signal.aborted && generation === sessionGeneration()) adminError.value = failure
  } finally {
    if (adminController === request) adminLoading.value = false
  }
}
async function saveAdministrator(): Promise<void> {
  if (adminLoading.value || adminSaving.value || busy.value || savedUsername.value === null) return
  adminValidation.value = null
  adminError.value = null
  const confirmed = newPassword.value === confirmPassword.value
  const update: AdministratorUpdate = { currentPassword: currentPassword.value }
  if (adminUsername.value !== savedUsername.value) update.username = adminUsername.value
  if (newPassword.value) update.newPassword = newPassword.value
  clearAdminPasswords()
  try {
    if (!confirmed) {
      adminValidation.value = 'passwordMismatch'
      return
    }
    if (update.username === undefined && !update.newPassword) {
      adminValidation.value = 'adminChangeRequired'
      return
    }
    adminController?.abort()
    const request = new AbortController()
    const generation = sessionGeneration()
    adminController = request
    adminSaving.value = true
    try {
      const result = await api.updateAdministrator(update, request.signal)
      if (!request.signal.aborted && generation === sessionGeneration()) {
        clearSession(result.username)
        notify('adminUpdated')
      }
    } catch (failure) {
      if (!request.signal.aborted && generation === sessionGeneration()) adminError.value = failure
    } finally {
      if (adminController === request) adminSaving.value = false
    }
  } finally {
    update.currentPassword = ''
    delete update.newPassword
    clearAdminPasswords()
  }
}
onMounted(loadAdministrator)
onBeforeUnmount(() => {
  adminController?.abort()
  clearAdminPasswords()
})
function confirmLogout(): void {
  error.value = null
  blocked.value = false
  logoutOpen.value = true
}
async function logout(): Promise<void> {
  if (busy.value || adminSaving.value || uploadsRunning.value || blocked.value) return
  busy.value = true
  error.value = null
  try {
    await signOut()
    logoutOpen.value = false
  } catch (failure) {
    error.value = failure
    noteError(failure)
    blocked.value = failure instanceof ApiError && failure.failure.code === 'outcome_uncertain'
  } finally {
    busy.value = false
  }
}
</script>
<template>
  <section class="view settings-view">
    <header class="view-heading">
      <div>
        <h1>{{ t('settings') }}</h1>
      </div>
    </header>
    <div class="settings-grid">
      <section class="settings-card server-card">
        <h2>
          <Server :size="20" aria-hidden="true" />
          {{ t('server') }}
        </h2>
        <CopyField :label="t('serverAddress')" :value="serverAddress" />
        <CopyField :label="t('webdavAddress')" :value="davAddress" />
      </section>
      <section class="settings-card download-card">
        <h2>
          <Link2 :size="20" aria-hidden="true" />
          {{ t('downloadSettings') }}
        </h2>
        <ErrorBanner
          v-if="downloadError"
          :error="downloadError"
          :busy="downloadBusy"
          @retry="loadDownloadSettings"
        />
        <p v-if="downloadLoading" class="field-help" role="status">{{ t('loading') }}</p>
        <template v-if="downloadSettings">
          <fieldset class="download-modes" :disabled="downloadBusy">
            <legend class="sr-only">{{ t('downloadSettings') }}</legend>
            <label v-for="mode in downloadModes" :key="mode.value" :class="{ selected: draftMode === mode.value }">
              <input
                type="radio"
                name="download-mode"
                :value="mode.value"
                :checked="draftMode === mode.value"
                @change="chooseDownloadMode(mode.value)"
              />
              <span>
                <strong>{{ t(mode.label) }}</strong>
                <small>{{ t(mode.help) }}</small>
              </span>
            </label>
          </fieldset>
          <p class="effective-mode"><span>{{ t('effectiveDownloadMode') }}</span><strong>{{ t(modeLabel(downloadSettings.mode)) }}</strong></p>
          <div v-if="draftMode === 'gdirect'" class="gdirect-status">
            <p class="selected-ip" role="status" :aria-busy="ipSelecting">
              <span v-if="ipSelecting" class="spinner small" aria-hidden="true"></span>
              <span>{{ t('gdirectConnection') }} · {{ ipSelecting ? t('gdirectConnecting') : status?.gdirect.ip || t('gdirectDisconnected') }}</span>
            </p>
            <button
              class="button outline"
              type="button"
              :disabled="downloadBusy || downloadSettings.mode !== 'gdirect'"
              @click="refreshIP"
            >
              <RefreshCw :size="16" aria-hidden="true" />
              {{ t('refreshIP') }}
            </button>
          </div>
          <form
            v-if="draftMode === 'proxy'"
            :aria-busy="downloadSaving"
            @submit.prevent="saveDownloadSettings"
          >
            <div class="worker-fields">
              <div class="field">
                <label for="setting-public-origin">{{ t('publicOrigin') }}</label>
                <input id="setting-public-origin" v-model="publicOrigin" type="url" required
                  autocomplete="off" spellcheck="false" :disabled="downloadBusy"
                  :aria-invalid="downloadValidation === 'publicOriginInvalid'" />
              </div>
              <div class="field">
                <label for="setting-worker-secret">{{ t('workerSecret') }}</label>
                <input
                  id="setting-worker-secret"
                  v-model="workerSecret"
                  type="password"
                  autocomplete="off"
                  spellcheck="false"
                  aria-describedby="setting-worker-configured setting-worker-secret-help"
                  :aria-invalid="downloadValidation === 'workerSecretInvalid' || downloadValidation === 'workerSecretRequired'"
                  :disabled="downloadBusy"
                />
                <small id="setting-worker-configured">
                  {{ t(downloadSettings.workerConfigured ? 'workerSecretConfigured' : 'workerSecretMissing') }}
                </small>
                <small id="setting-worker-secret-help">{{ t('workerSecretHelp') }}</small>
              </div>
              <div class="field">
                <label for="setting-worker-url">{{ t('workerURL') }}</label>
                <input
                  id="setting-worker-url"
                  v-model="workerURL"
                  type="url"
                  required
                  autocomplete="off"
                  spellcheck="false"
                  :aria-invalid="downloadValidation === 'workerURLInvalid'"
                  :disabled="downloadBusy"
                />
              </div>
            </div>
            <p v-if="proxyHTTP" class="field-help">{{ t('proxyHTTPWarning') }}</p>
            <p v-if="downloadValidation" class="field-error" role="alert">{{ t(downloadValidation) }}</p>
            <div class="settings-actions proxy-actions">
              <button class="button outline" type="button" :disabled="downloadBusy" @click="copyWorkerScript">
                {{ scriptCopying ? t('working') : t('copyWorkerScript') }}
              </button>
              <button class="button tonal" type="submit" :disabled="downloadBusy">
                {{ downloadSaving ? t('working') : t('save') }}
              </button>
            </div>
          </form>
        </template>
      </section>
      <UploadSettingsCard />
      <section class="settings-card">
        <h2>
          <Moon :size="20" aria-hidden="true" />
          {{ t('appearance') }}
        </h2>
        <div class="field">
          <span class="field-label" id="setting-language-label">
            <Globe :size="16" aria-hidden="true" />
            {{ t('language') }}
          </span>
          <div class="segmented" role="group" aria-labelledby="setting-language-label">
            <button
              v-for="option in languageOptions"
              :key="option.value"
              type="button"
              class="button"
              :class="language === option.value ? 'tonal' : 'text'"
              :aria-pressed="language === option.value"
              @click="language = option.value"
            >{{ option.label }}</button>
          </div>
        </div>
        <div class="field">
          <span class="field-label" id="setting-theme-label">{{ t('theme') }}</span>
          <div class="segmented" role="group" aria-labelledby="setting-theme-label">
            <button
              v-for="option in themeOptions"
              :key="option.value"
              type="button"
              class="button"
              :class="theme === option.value ? 'tonal' : 'text'"
              :aria-pressed="theme === option.value"
              @click="theme = option.value"
            >{{ option.label }}</button>
          </div>
        </div>
      </section>
      <section class="settings-card">
        <h2>
          <UserRound :size="20" aria-hidden="true" />
          {{ t('account') }}
        </h2>
        <ErrorBanner
          v-if="adminError"
          :error="adminError"
          :busy="adminLoading || adminSaving"
          @retry="loadAdministrator"
        />
        <p v-if="adminLoading" class="field-help" role="status">{{ t('loading') }}</p>
        <form
          v-if="savedUsername !== null"
          id="administrator-settings"
          :aria-busy="adminLoading || adminSaving"
          @submit.prevent="saveAdministrator"
        >
          <div class="admin-fields">
            <div class="field">
              <label for="setting-admin-username">{{ t('username') }}</label>
              <input
                id="setting-admin-username"
                v-model="adminUsername"
                required
                autocomplete="username"
                autocapitalize="none"
                spellcheck="false"
                :disabled="adminLoading || adminSaving"
              />
            </div>
            <div class="field">
              <label for="setting-admin-current">{{ t('currentPassword') }}</label>
              <input
                id="setting-admin-current"
                v-model="currentPassword"
                type="password"
                required
                autocomplete="current-password"
                :disabled="adminLoading || adminSaving"
              />
            </div>
            <div class="field">
              <label for="setting-admin-new">{{ t('newPassword') }}</label>
              <input
                id="setting-admin-new"
                v-model="newPassword"
                type="password"
                autocomplete="new-password"
                aria-describedby="setting-admin-password-help"
                :aria-invalid="adminValidation === 'passwordMismatch'"
                :disabled="adminLoading || adminSaving"
              />
              <small id="setting-admin-password-help">{{ t('adminPasswordHelp') }}</small>
            </div>
            <div class="field">
              <label for="setting-admin-confirm">{{ t('confirmPassword') }}</label>
              <input
                id="setting-admin-confirm"
                v-model="confirmPassword"
                type="password"
                autocomplete="new-password"
                :aria-invalid="adminValidation === 'passwordMismatch'"
                :disabled="adminLoading || adminSaving"
              />
            </div>
          </div>
          <p class="field-help">{{ t('adminReloginHelp') }}</p>
          <p v-if="adminValidation" class="field-error" role="alert">{{ t(adminValidation) }}</p>
        </form>
        <div class="settings-actions admin-actions">
          <button
            class="button text"
            type="button"
            :disabled="uploadsRunning || adminSaving || busy"
            :title="uploadsRunning ? t('uploadBusy') : undefined"
            @click="confirmLogout"
          >
            <LogOut :size="18" aria-hidden="true" />
            {{ t('logout') }}
          </button>
          <button
            v-if="savedUsername !== null"
            class="button filled"
            type="submit"
            form="administrator-settings"
            :disabled="adminLoading || adminSaving || busy || !currentPassword || !adminChanged"
          >
            {{ adminSaving ? t('working') : t('save') }}
          </button>
        </div>
      </section>
      <section class="settings-card about-card">
        <h2>{{ t('about') }}</h2>
        <div class="about-row">
          <strong>GuluDrive</strong>
          <span class="muted">{{ status?.version || '—' }}</span>
        </div>
      </section>
    </div>
    <BaseDialog v-if="logoutOpen" :title="t('logout')" :busy="busy" @close="logoutOpen = false">
      <form @submit.prevent="logout">
        <p>{{ t('confirmLogout') }}</p>
        <p v-if="error" class="field-error" role="alert">{{ describeError(error) }}</p>
        <footer class="dialog-actions">
          <button
            class="button text"
            type="button"
            :disabled="busy"
            @click="logoutOpen = false"
          >
            {{ t('cancel') }}
          </button>
          <button class="button filled" type="submit" :disabled="busy || blocked">
            {{ busy ? t('working') : t('logout') }}
          </button>
        </footer>
      </form>
    </BaseDialog>
  </section>
</template>
