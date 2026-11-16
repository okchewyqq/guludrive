<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { ArrowRight, Container } from '@lucide/vue'
import { t, type MessageKey } from '../lib/i18n'
import { validPasswordLength } from '../lib/password'
import { ApiError, sessionGeneration } from '../api/client'
import { describeError, noteError } from '../state/feedback'
import { initialized, initializeAdministrator, loginUsername, signIn } from '../state/session'

const setup = computed(() => initialized.value === false)
const username = ref(setup.value ? 'admin' : loginUsername.value)
const password = ref('')
const confirmPassword = ref('')
const busy = ref(false)
const error = ref<unknown>(null)
const validation = ref<MessageKey | null>(null)
const controller = new AbortController()
async function submit(): Promise<void> {
  if (busy.value || !username.value.trim() || !password.value) return
  validation.value = null
  error.value = null
  if (setup.value && !validPasswordLength(password.value)) {
    validation.value = 'passwordLengthInvalid'
    return
  }
  if (setup.value && password.value !== confirmPassword.value) {
    validation.value = 'passwordMismatch'
    return
  }
  let secret = password.value
  password.value = ''
  confirmPassword.value = ''
  busy.value = true
  const generation = sessionGeneration()
  try {
    if (setup.value) await initializeAdministrator(username.value.trim(), secret, controller.signal)
    else await signIn(username.value.trim(), secret, controller.signal)
  } catch (failure) {
    if (!controller.signal.aborted && (generation === sessionGeneration() ||
      (failure instanceof ApiError && failure.status === 401 && generation + 1 === sessionGeneration()))) {
      error.value = failure
      noteError(failure)
    }
  } finally {
    secret = ''
    busy.value = false
  }
}
onBeforeUnmount(() => {
  controller.abort()
  password.value = ''
  confirmPassword.value = ''
})
</script>
<template>
  <main class="auth-page">
    <section class="login-panel">
      <div class="login-brand">
        <span class="brand-icon" aria-hidden="true"><Container :size="24" /></span>
        <span>GuluDrive</span>
      </div>
      <div class="login-heading"><h1>{{ t(setup ? 'setupTitle' : 'loginTitle') }}</h1></div>
      <form @submit.prevent="submit">
        <div class="field">
          <label for="admin-user">{{ t('username') }}</label>
          <input id="admin-user" v-model="username" autofocus required autocomplete="username"
            autocapitalize="none" spellcheck="false" :disabled="busy" />
        </div>
        <div class="field">
          <label for="admin-password">{{ t('password') }}</label>
          <input id="admin-password" v-model="password" type="password" required
            :autocomplete="setup ? 'new-password' : 'current-password'" :disabled="busy"
            :aria-invalid="validation === 'passwordLengthInvalid'"
            :aria-describedby="validation ? 'auth-validation' : undefined" />
        </div>
        <div v-if="setup" class="field">
          <label for="admin-confirm">{{ t('setupConfirmPassword') }}</label>
          <input id="admin-confirm" v-model="confirmPassword" type="password" required
            autocomplete="new-password" :disabled="busy" :aria-invalid="validation === 'passwordMismatch'"
            :aria-describedby="validation ? 'auth-validation' : undefined" />
        </div>
        <p v-if="validation" id="auth-validation" class="field-error" role="alert">{{ t(validation) }}</p>
        <p v-if="error" class="field-error" role="alert">{{ describeError(error) }}</p>
        <button class="button filled login-submit" type="submit"
          :disabled="busy || !username.trim() || !password || (setup && !confirmPassword)">
          <span v-if="busy" class="spinner small" aria-hidden="true"></span>
          {{ busy ? t('working') : t(setup ? 'createAdministrator' : 'login') }}
          <ArrowRight v-if="!busy" :size="18" aria-hidden="true" />
        </button>
      </form>
    </section>
  </main>
</template>
