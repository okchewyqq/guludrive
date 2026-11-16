<script setup lang="ts">
import { ChevronDown, ChevronUp, Upload, X } from '@lucide/vue'
import { ref } from 'vue'
import { t } from '../lib/i18n'
import { describeError } from '../state/feedback'
import {
  cancelUpload,
  clearFinishedUploads,
  uploadCompleted,
  uploadItems,
  uploadsRunning,
} from '../state/uploads'
const collapsed = ref(false)
</script>
<template>
  <section v-if="uploadItems.length" class="upload-queue" :aria-label="t('uploads')">
    <header>
      <button class="upload-summary" :aria-expanded="!collapsed" @click="collapsed = !collapsed">
        <Upload :size="18" aria-hidden="true" />
        <strong>{{ t('uploads') }}</strong>
        <span>{{ uploadCompleted }} / {{ uploadItems.length }}</span>
        <ChevronDown v-if="collapsed" :size="17" aria-hidden="true" />
        <ChevronUp v-else :size="17" aria-hidden="true" />
      </button>
      <button
        v-if="!uploadsRunning"
        class="icon-button"
        :aria-label="t('clearFinished')"
        @click="clearFinishedUploads"
      >
        <X :size="18" aria-hidden="true" />
      </button>
    </header>
    <div v-show="!collapsed" class="upload-items">
      <div v-for="item in uploadItems" :key="item.id" class="upload-item">
        <div class="upload-item-top">
          <span class="truncate" :title="item.path">{{ item.name }}</span>
          <span class="muted">
            {{ t(item.state === 'cancelled' ? 'uploadCancelled' : item.state) }}
          </span>
          <button
            v-if="['queued', 'uploading', 'publishing'].includes(item.state)"
            class="icon-button small"
            :aria-label="t('stopUpload') + ' · ' + item.name"
            @click="cancelUpload(item.id)"
          >
            <X :size="16" aria-hidden="true" />
          </button>
        </div>
        <progress
          v-if="['uploading', 'publishing'].includes(item.state)"
          :value="item.percent"
          max="100"
          :aria-label="t('uploadProgress', { name: item.name, percent: item.percent })"
        ></progress>
        <p v-if="item.error" class="field-error">{{ describeError(item.error) }}</p>
      </div>
    </div>
  </section>
</template>
