<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'
import { ExternalLink } from '@lucide/vue'
import BaseDialog from './BaseDialog.vue'
import CopyField from './CopyField.vue'
import { api, ApiError } from '../api/client'
import { t } from '../lib/i18n'
import { describeError, noteError, notify } from '../state/feedback'

const emit = defineEmits<{ close: []; connected: [] }>()
const authorizationURL = 'https://accounts.google.com/EmbeddedSetup'
const token = ref('')
const busy = ref(false)
const error = ref<unknown>(null)
const blocked = ref(false)
function close(): void {
  token.value = ''
  emit('close')
}
async function submit(): Promise<void> {
  if (busy.value || blocked.value || !token.value.trim()) return
  const value = token.value.trim()
  token.value = ''
  error.value = null
  busy.value = true
  try {
    await api.connectGoogle(value)
    notify('operationDone')
    emit('connected')
    close()
  } catch (failure) {
    error.value = failure
    noteError(failure)
    blocked.value = failure instanceof ApiError && failure.failure.code === 'outcome_uncertain'
  } finally {
    busy.value = false
  }
}
onBeforeUnmount(() => {
  token.value = ''
})
</script>
<template>
  <BaseDialog :title="t('connectAccount')" :busy="busy" @close="close">
    <form @submit.prevent="submit">
      <ol class="auth-steps">
        <li>
          <h3>{{ t('googleStepOne') }}</h3>
          <CopyField :label="t('authorizationURL')" :value="authorizationURL" />
          <a
            :href="authorizationURL"
            target="_blank"
            rel="noopener noreferrer"
            class="button outline"
          >
            <ExternalLink :size="17" aria-hidden="true" />
            {{ t('openGoogle') }}
          </a>
        </li>
        <li>
          <h3>{{ t('googleStepTwo') }}</h3>
          <div class="field">
            <label for="google-token">{{ t('oauthToken') }}</label>
            <input
              id="google-token"
              v-model="token"
              type="password"
              autofocus
              required
              autocomplete="off"
              spellcheck="false"
              autocapitalize="none"
              :disabled="busy || blocked"
              aria-describedby="token-help"
            />
            <small id="token-help">{{ t('tokenHelp') }}</small>
          </div>
        </li>
      </ol>
      <details class="short-help">
        <summary>{{ t('googleHelp') }}</summary>
        <p>{{ t('cookieHelp') }}</p>
        <p>{{ t('consentNote') }}</p>
      </details>
      <p v-if="error" class="field-error" role="alert">{{ describeError(error) }}</p>
      <footer class="dialog-actions">
        <button class="button text" type="button" :disabled="busy" @click="close">
          {{ t('cancel') }}
        </button>
        <button class="button filled" type="submit" :disabled="busy || blocked || !token.trim()">
          <span v-if="busy" class="spinner small" aria-hidden="true"></span>
          {{ busy ? t('authorizing') : t('connect') }}
        </button>
      </footer>
    </form>
  </BaseDialog>
</template>
