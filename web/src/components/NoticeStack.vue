<script setup lang="ts">
import { AlertCircle, CheckCircle2, X } from '@lucide/vue'
import { describeError, dismissNotice, notices } from '../state/feedback'
import { t } from '../lib/i18n'
</script>
<template>
  <div class="notice-stack" aria-live="polite" aria-atomic="false">
    <TransitionGroup name="notice">
      <div
        v-for="notice in notices"
        :key="notice.id"
        class="notice"
        :class="{ 'notice-error': notice.error }"
      >
        <AlertCircle v-if="notice.error" :size="19" aria-hidden="true" />
        <CheckCircle2 v-else :size="19" aria-hidden="true" />
        <span>
          {{ notice.error ? describeError(notice.error) : notice.message ? t(notice.message, notice.values ?? {}) : '' }}
        </span>
        <button class="icon-button" :aria-label="t('dismiss')" @click="dismissNotice(notice.id)">
          <X :size="17" aria-hidden="true" />
        </button>
      </div>
    </TransitionGroup>
  </div>
</template>
