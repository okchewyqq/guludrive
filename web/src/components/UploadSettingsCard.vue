<script setup lang="ts">
import { Upload } from '@lucide/vue'
import ErrorBanner from './ErrorBanner.vue'
import { t } from '../lib/i18n'
import { useUploadSettings } from '../state/uploadSettings'

const {
  uploadSettings, largeFileFormat, mp4GB, bmpMB, uploadLoading, uploadSaving,
  uploadBusy, uploadChanged, uploadError, uploadValidation, loadUploadSettings, saveUploadSettings,
} = useUploadSettings()
</script>
<template>
  <section class="settings-card upload-card">
    <h2><Upload :size="20" aria-hidden="true" />{{ t('uploadSettings') }}</h2>
    <ErrorBanner v-if="uploadError" :error="uploadError" :busy="uploadBusy" @retry="loadUploadSettings" />
    <p v-if="uploadLoading" class="field-help" role="status">{{ t('loading') }}</p>
    <form v-if="uploadSettings" :aria-busy="uploadBusy" novalidate @submit.prevent="saveUploadSettings">
      <fieldset class="download-modes upload-modes" :disabled="uploadBusy">
        <legend>{{ t('largeFileUpload') }}</legend>
        <label :class="{ selected: largeFileFormat === 'bmp' }">
          <input v-model="largeFileFormat" type="radio" name="large-file-format" value="bmp" />
          <strong>{{ t('uploadBMP') }}</strong>
        </label>
        <label :class="{ selected: largeFileFormat === 'mp4' }">
          <input v-model="largeFileFormat" type="radio" name="large-file-format" value="mp4" />
          <strong>{{ t('uploadMP4') }}</strong>
        </label>
      </fieldset>
      <p class="field-help upload-policy">{{ t('uploadPolicyHelp') }}</p>
      <p v-if="largeFileFormat === 'mp4'" class="field-help">{{ t('mp4Warning') }}</p>
      <div class="upload-fields">
        <div class="field">
          <label for="setting-upload-bmp">{{ t('bmpPartSize') }}</label>
          <input
            id="setting-upload-bmp" v-model="bmpMB" type="number" min="100" max="150" step="1"
            inputmode="decimal" required :disabled="uploadBusy" :aria-invalid="uploadValidation"
            :aria-describedby="uploadValidation ? 'upload-settings-validation' : undefined"
          />
        </div>
        <div class="field">
          <label for="setting-upload-mp4">{{ t('mp4PartSize') }}</label>
          <input
            id="setting-upload-mp4" v-model="mp4GB" type="number" min="1" max="9" step="0.1"
            inputmode="decimal" required :disabled="uploadBusy" :aria-invalid="uploadValidation"
            :aria-describedby="uploadValidation ? 'upload-settings-validation' : undefined"
          />
        </div>
      </div>
      <p v-if="uploadValidation" id="upload-settings-validation" class="field-error" role="alert">
        {{ t('uploadSettingsInvalid') }}
      </p>
      <div class="settings-actions">
        <button class="button tonal" type="submit" :disabled="uploadBusy || !uploadChanged">
          {{ uploadSaving ? t('working') : t('save') }}
        </button>
      </div>
    </form>
  </section>
</template>
