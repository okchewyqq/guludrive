<script setup lang="ts">
import { ref } from 'vue'
import { Check, Plus, Trash2, UserRound } from '@lucide/vue'
import BaseDialog from '../components/BaseDialog.vue'
import GoogleConnectDialog from '../components/GoogleConnectDialog.vue'
import { api, ApiError } from '../api/client'
import type { Account } from '../api/models'
import { t } from '../lib/i18n'
import { describeError, noteError, notify, reportError } from '../state/feedback'
import { accounts, buckets, refreshWorkspace, workspaceLoading } from '../state/workspace'
import { uploadsRunning } from '../state/uploads'

const connectOpen = ref(false)
const pendingID = ref<string | null>(null)
const removeTarget = ref<Account | null>(null)
const removeError = ref<unknown>(null)
const blocked = ref(false)
function bucketCount(accountID: string): number {
  return buckets.value.filter((bucket) => bucket.album.accountID === accountID).length
}
async function refresh(): Promise<void> {
  try {
    await refreshWorkspace()
  } catch (error) {
    reportError(error)
  }
}
async function select(account: Account): Promise<void> {
  if (pendingID.value || uploadsRunning.value) return
  pendingID.value = account.id
  try {
    await api.selectAccount(account.id)
    notify('operationDone')
    await refresh()
  } catch (error) {
    reportError(error)
  } finally {
    pendingID.value = null
  }
}
function confirmRemove(account: Account): void {
  removeTarget.value = account
  removeError.value = null
  blocked.value = false
}
async function remove(): Promise<void> {
  if (!removeTarget.value || pendingID.value || blocked.value) return
  pendingID.value = removeTarget.value.id
  try {
    await api.removeAccount(removeTarget.value.id)
    removeTarget.value = null
    notify('operationDone')
    await refresh()
  } catch (error) {
    removeError.value = error
    noteError(error)
    blocked.value = error instanceof ApiError && error.failure.code === 'outcome_uncertain'
  } finally {
    pendingID.value = null
  }
}
</script>
<template>
  <section class="view accounts-view">
    <header class="view-heading">
      <h1>{{ t('accounts') }}</h1>
      <div class="heading-actions">
        <button
          type="button"
          class="button filled"
          :disabled="!!pendingID || uploadsRunning || workspaceLoading"
          @click="connectOpen = true"
        >
          <Plus :size="20" aria-hidden="true" />
          {{ t('connectAccount') }}
        </button>
      </div>
    </header>
    <div v-if="workspaceLoading && !accounts.length" class="loading-state" role="status">
      <span class="spinner" aria-hidden="true"></span>
      {{ t('loading') }}
    </div>
    <div v-else-if="accounts.length" class="account-list">
      <article v-for="account in accounts" :key="account.id" class="account-card">
        <div class="account-avatar"><UserRound :size="24" aria-hidden="true" /></div>
        <div class="account-info">
          <h2 :title="account.email">{{ account.email }}</h2>
          <span class="muted small-text">{{ t('accountID', { id: account.id }) }}</span>
        </div>
        <span v-if="account.selected" class="pill">{{ t('selectedAccount') }}</span>
        <button
          v-else
          type="button"
          class="button tonal"
          :disabled="!!pendingID || uploadsRunning"
          @click="select(account)"
        >
          <span v-if="pendingID === account.id" class="spinner small" aria-hidden="true"></span>
          <Check v-else :size="20" aria-hidden="true" />
          {{ t('useAccount') }}
        </button>
        <button
          type="button"
          class="icon-button"
          :aria-label="t('removeAccount') + ' · ' + account.email"
          :title="t('removeAccount')"
          :disabled="!!pendingID || uploadsRunning"
          @click="confirmRemove(account)"
        >
          <Trash2 :size="22" aria-hidden="true" />
        </button>
        <p>{{ t('bucketCount', { count: bucketCount(account.id) }) }}</p>
      </article>
    </div>
    <div v-else class="account-card">
      <div class="account-info">
        <h2>{{ t('noAccountsTitle') }}</h2>
        <span class="muted small-text">{{ t('connectGoogleHint') }}</span>
      </div>
    </div>
    <GoogleConnectDialog v-if="connectOpen" @close="connectOpen = false" @connected="refresh" />
    <BaseDialog
      v-if="removeTarget"
      :title="t('removeAccountTitle')"
      :busy="!!pendingID"
      @close="removeTarget = null"
    >
      <form @submit.prevent="remove">
        <p class="break-path">{{ removeTarget.email }}</p>
        <p class="muted">{{ t('removeAccountHelp') }}</p>
        <p v-if="removeError" class="field-error" role="alert">{{ describeError(removeError) }}</p>
        <footer class="dialog-actions">
          <button
            class="button text"
            type="button"
            :disabled="!!pendingID"
            @click="removeTarget = null"
          >
            {{ t('cancel') }}
          </button>
          <button class="button danger" type="submit" :disabled="!!pendingID || blocked">
            <span v-if="pendingID" class="spinner small" aria-hidden="true"></span>
            {{ pendingID ? t('removing') : t('removeAccount') }}
          </button>
        </footer>
      </form>
    </BaseDialog>
  </section>
</template>
