<script setup lang="ts">
import { AlertCircle, RefreshCw } from '@lucide/vue'
import { describeError } from '../state/feedback'
import { t } from '../lib/i18n'
import { ApiError } from '../api/client'
defineProps<{ error: unknown; busy?: boolean }>()
defineEmits<{ retry: [] }>()
</script>
<template>
  <div class="error-banner" role="alert">
    <AlertCircle :size="20" aria-hidden="true" />
    <span>{{ describeError(error) }}</span>
    <button
      v-if="!(error instanceof ApiError && error.failure.code === 'outcome_uncertain')"
      class="button text"
      :disabled="busy"
      @click="$emit('retry')"
    >
      <RefreshCw :size="16" aria-hidden="true" />
      {{ t('retry') }}
    </button>
  </div>
</template>
