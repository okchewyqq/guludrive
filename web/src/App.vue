<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { AlertTriangle, Container, Menu, X } from '@lucide/vue'
import AppNavigation from './components/AppNavigation.vue'
import BaseDialog from './components/BaseDialog.vue'
import BrandMark from './components/BrandMark.vue'
import ErrorBanner from './components/ErrorBanner.vue'
import NoticeStack from './components/NoticeStack.vue'
import UploadQueue from './components/UploadQueue.vue'
import AccountsView from './views/AccountsView.vue'
import FilesView from './views/FilesView.vue'
import LoginView from './views/LoginView.vue'
import SettingsView from './views/SettingsView.vue'
import { t } from './lib/i18n'
import { reportError, uncertainOutcome } from './state/feedback'
import { route } from './state/navigation'
import {
  authenticated, refreshStatus, restoreError, restoreSession, restoring, status,
} from './state/session'
import {
  clearWorkspace,
  refreshWorkspace,
  workspaceError,
  workspaceLoading,
} from './state/workspace'

const menuOpen = ref(false)
const refreshing = ref(false)
const main = ref<HTMLElement>()
const currentView = computed(() =>
  route.value.view === 'accounts'
    ? AccountsView
    : route.value.view === 'settings'
      ? SettingsView
      : FilesView,
)
const busy = computed(() => refreshing.value || workspaceLoading.value)
const busyLabel = computed(() => (refreshing.value ? t('refreshing') : t('working')))
async function refresh(): Promise<void> {
  refreshing.value = true
  try {
    await refreshWorkspace()
  } catch (error) {
    reportError(error)
  } finally {
    refreshing.value = false
  }
}
watch(authenticated, (ready) => {
  if (ready) void refresh()
  else {
    clearWorkspace()
    menuOpen.value = false
  }
})
watch(route, async () => {
  menuOpen.value = false
  await nextTick()
  main.value?.focus({ preventScroll: true })
})
// Startup selection is asynchronous. Poll only while the server says it is selecting.
watch(
  () => authenticated.value && !!status.value?.gdirect.enabled && status.value.gdirect.selecting,
  (selecting, _previous, cleanup) => {
    if (!selecting) return
    const request = new AbortController()
    let timer: ReturnType<typeof setTimeout>
    const poll = async () => {
      try {
        await refreshStatus(request.signal)
        if (!request.signal.aborted && status.value?.gdirect.selecting)
          timer = setTimeout(poll, 750)
      } catch (failure) {
        if (!request.signal.aborted) reportError(failure)
      }
    }
    timer = setTimeout(poll, 750)
    cleanup(() => {
      request.abort()
      clearTimeout(timer)
    })
  },
)
onMounted(restoreSession)
</script>
<template>
  <div v-if="restoring" class="app-start" role="status">
    <BrandMark />
    <span class="spinner" aria-hidden="true"></span>
    <p>{{ t('connecting') }}</p>
  </div>
  <main v-else-if="restoreError" class="app-start">
    <BrandMark />
    <h1>{{ t('serviceUnavailable') }}</h1>
    <ErrorBanner :error="restoreError" @retry="restoreSession" />
  </main>
  <LoginView v-else-if="!authenticated" />
  <div v-else class="app-shell">
    <a class="skip-link" href="#main-content" @click.prevent="main?.focus()">
      {{ t('skipContent') }}
    </a>
    <header class="top-bar">
      <button class="icon-button menu-button" :aria-label="t('menu')" @click="menuOpen = true">
        <Menu :size="22" aria-hidden="true" />
      </button>
      <span class="brand-icon" aria-hidden="true"><Container :size="24" /></span>
      <span class="brand">GuluDrive</span>
      <span class="top-bar-spacer"></span>
      <span
        v-if="status?.gdirect.enabled"
        class="gdirect-badge"
        :title="t('gdirectConnection')"
        role="status"
      >
        <span class="status-dot" :class="{ ready: !status.gdirect.selecting && !!status.gdirect.ip }" aria-hidden="true"></span>
        <span>{{ t('gdirect') }} · {{ status.gdirect.selecting ? t('gdirectConnecting') : status.gdirect.ip || t('gdirectDisconnected') }}</span>
      </span>
      <span v-if="busy" class="busy-label" role="status">
        <span class="spinner small" aria-hidden="true"></span>
        <span class="busy-text">{{ busyLabel }}</span>
      </span>
    </header>
    <div class="app-body">
      <aside class="desktop-sidebar"><AppNavigation /></aside>
      <div class="workspace">
        <main id="main-content" ref="main" class="main-content" tabindex="-1">
          <div v-if="uncertainOutcome" class="uncertain-banner" role="alert">
            <AlertTriangle :size="20" aria-hidden="true" />
            <span>{{ t('uncertain') }}</span>
            <button class="icon-button small" :aria-label="t('dismiss')" @click="uncertainOutcome = false">
              <X :size="18" aria-hidden="true" />
            </button>
          </div>
          <ErrorBanner
            v-if="workspaceError"
            :error="workspaceError"
            :busy="workspaceLoading"
            @retry="refresh"
          />
          <Transition name="view" mode="out-in">
            <component :is="currentView" :key="route.view" />
          </Transition>
        </main>
      </div>
    </div>
    <BaseDialog v-if="menuOpen" class="navigation-modal" title="GuluDrive" @close="menuOpen = false">
      <AppNavigation @navigate="menuOpen = false" />
    </BaseDialog>
    <UploadQueue />
  </div>
  <NoticeStack />
</template>
