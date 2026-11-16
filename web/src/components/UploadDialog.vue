<script setup lang="ts">
import { computed, ref } from 'vue'
import { FileUp, X } from '@lucide/vue'
import BaseDialog from './BaseDialog.vue'
import type { Entry } from '../api/models'
import { formatSize, t } from '../lib/i18n'
import { validName } from '../lib/paths'
import { enqueueUploads } from '../state/uploads'

const props = defineProps<{ files: File[]; entries: Entry[]; path: string }>()
const emit = defineEmits<{ close: [] }>()
const files = ref(props.files.map((file, id) => ({ file, id })))
const overwrite = ref(false)
const candidates = computed(() =>
  files.value.map(({ file, id }) => {
    const existing = props.entries.find((entry) => entry.name === file.name)
    const invalid =
      !validName(file.name) ||
      !!existing?.isDir ||
      files.value.filter((item) => item.file.name === file.name).length > 1
    return { file, existing, invalid, id }
  }),
)
const hasConflict = computed(() => candidates.value.some((item) => item.existing && !item.invalid))
const hasInvalid = computed(() => candidates.value.some((item) => item.invalid))
const total = computed(() => files.value.reduce((sum, item) => sum + item.file.size, 0))
function start(): void {
  if (!candidates.value.length || hasInvalid.value || (hasConflict.value && !overwrite.value))
    return
  enqueueUploads(
    candidates.value.map(({ file, existing }) => ({ file, existing })),
    props.path,
  )
  emit('close')
}
</script>
<template>
  <BaseDialog :title="t('uploadTitle')" @close="emit('close')">
    <form @submit.prevent="start">
      <p class="muted break-path">{{ t('uploadDestination', { path }) }}</p>
      <p class="small-text">
        {{ t('uploadSummary', { count: files.length, size: formatSize(total) }) }}
      </p>
      <div class="upload-review">
        <div v-for="item in candidates" :key="item.id" class="upload-review-item">
          <FileUp :size="21" aria-hidden="true" />
          <div>
            <strong class="break-path">{{ item.file.name }}</strong>
            <span class="muted small-text">{{ formatSize(item.file.size) }}</span>
            <span v-if="item.invalid" class="field-error">{{ t('invalidUpload') }}</span>
            <span v-else-if="item.existing" class="warning-text small-text">
              {{ t('existingFile') }}
            </span>
          </div>
          <button
            type="button"
            class="icon-button"
            :aria-label="t('removeFromList', { name: item.file.name })"
            @click="files = files.filter((file) => file.id !== item.id)"
          >
            <X :size="17" aria-hidden="true" />
          </button>
        </div>
      </div>
      <label v-if="hasConflict" class="checkbox-label">
        <input v-model="overwrite" type="checkbox" />
        {{ t('replaceFiles') }}
      </label>
      <footer class="dialog-actions">
        <button type="button" class="button text" @click="emit('close')">
          {{ t('cancel') }}
        </button>
        <button
          type="submit"
          class="button filled"
          :disabled="!files.length || hasInvalid || (hasConflict && !overwrite)"
        >
          {{ t('startUpload') }}
        </button>
      </footer>
    </form>
  </BaseDialog>
</template>
