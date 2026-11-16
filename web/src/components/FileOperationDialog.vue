<script setup lang="ts">
import { computed, ref } from 'vue'
import BaseDialog from './BaseDialog.vue'
import { api, ApiError } from '../api/client'
import type { Entry } from '../api/models'
import { t, type MessageKey } from '../lib/i18n'
import { joinPath, parentPath, validDestination, validName } from '../lib/paths'
import type { FileOperation } from '../lib/operations'
import { describeError, noteError, notify } from '../state/feedback'

const props = defineProps<{ operation: FileOperation; entries: Entry[]; directory: string }>()
const emit = defineEmits<{ close: []; changed: [] }>()
const name = ref(props.operation === 'rename' ? (props.entries[0]?.name ?? '') : '')
// Batch move/copy takes a destination folder; a single entry keeps pre-filling its own path.
const batch = computed(() => props.entries.length > 1)
const destination = ref(batch.value ? '' : (props.entries[0]?.path ?? ''))
const overwrite = ref(false)
const busy = ref(false)
const progress = ref(0)
const error = ref<unknown>(null)
const invalid = ref<MessageKey | null>(null)
const blocked = ref(false)
const titles: Record<FileOperation, MessageKey> = {
  createBucket: 'newBucket',
  createDirectory: 'newDirectory',
  rename: 'rename',
  move: 'move',
  copy: 'copy',
  delete: 'deleteTitle',
}
const needsName = computed(() =>
  ['createBucket', 'createDirectory', 'rename'].includes(props.operation),
)
const needsDestination = computed(
  () => props.operation === 'move' || props.operation === 'copy',
)
const helpKey = computed<MessageKey>(() => {
  if (needsDestination.value && batch.value) return 'destinationFolderHelp'
  return 'destinationHelp'
})
function entryDestination(entry: Entry, target: string): string {
  return batch.value ? joinPath(target, entry.name) : target
}
function validTarget(target: string): boolean {
  if (!validDestination(target)) return false
  return !props.entries.some((entry) => entryDestination(entry, target) === entry.path)
}
async function submit(): Promise<void> {
  if (busy.value || blocked.value) return
  invalid.value = null
  error.value = null
  const nextName = name.value.trim()
  const target = destination.value.trim()
  if (needsName.value && !validName(nextName)) {
    invalid.value = 'invalidName'
    return
  }
  if (needsDestination.value && !validTarget(target)) {
    invalid.value = 'invalidDestination'
    return
  }
  busy.value = true
  progress.value = 0
  let changed = false
  try {
    const source = props.entries[0]
    switch (props.operation) {
      case 'createBucket':
        await api.createBucket(nextName)
        break
      case 'createDirectory':
        await api.createDirectory(joinPath(props.directory, nextName))
        break
      case 'rename':
        if (!source || source.name === nextName) {
          emit('close')
          return
        }
        await api.transfer(
          'move',
          source,
          joinPath(parentPath(source.path), nextName),
          overwrite.value,
        )
        break
      case 'move':
      case 'copy':
        if (!props.entries.length) return
        if (batch.value) {
          // Sequential, one entry at a time: the first failure stops the batch.
          for (const entry of props.entries) {
            await api.transfer(
              props.operation,
              entry,
              entryDestination(entry, target),
              overwrite.value,
            )
            changed = true
            progress.value++
          }
        } else {
          const only = props.entries[0]
          await api.transfer(props.operation, only, target, overwrite.value)
          progress.value = 1
        }
        break
      case 'delete':
        for (const entry of props.entries) {
          await api.remove(entry)
          changed = true
          progress.value++
        }
        break
    }
    changed = true
    notify('operationDone')
    emit('close')
  } catch (failure) {
    error.value = failure
    noteError(failure)
    blocked.value =
      changed || (failure instanceof ApiError && failure.failure.code === 'outcome_uncertain')
  } finally {
    busy.value = false
    if (changed || blocked.value) emit('changed')
  }
}
</script>
<template>
  <BaseDialog :title="t(titles[operation])" :busy="busy" @close="emit('close')">
    <form @submit.prevent="submit">
      <p v-if="operation === 'createBucket'" class="muted">{{ t('bucketHelp') }}</p>
      <p v-else-if="operation === 'createDirectory'" class="muted">{{ t('directoryHelp') }}</p>
      <p v-else-if="operation === 'delete'" class="muted">{{ t('deleteHelp') }}</p>
      <ul v-if="entries.length" class="operation-sources">
        <li v-for="entry in entries" :key="entry.id" class="break-path">{{ entry.path }}</li>
      </ul>
      <div v-if="needsName" class="field">
        <label for="operation-name">{{ t('name') }}</label>
        <input
          id="operation-name"
          v-model="name"
          autofocus
          required
          maxlength="255"
          autocomplete="off"
          :disabled="busy || blocked"
          :aria-invalid="!!invalid"
          aria-describedby="name-help"
        />
        <small id="name-help">{{ t('nameHelp') }}</small>
      </div>
      <div v-else-if="needsDestination" class="field">
        <label for="operation-destination">
          {{ batch ? t('destinationFolder') : t('destination') }}
        </label>
        <input
          id="operation-destination"
          v-model="destination"
          autofocus
          required
          autocomplete="off"
          :disabled="busy || blocked"
          :aria-invalid="!!invalid"
          aria-describedby="destination-help"
        />
        <small id="destination-help">{{ t(helpKey) }}</small>
      </div>
      <label v-if="['rename', 'move', 'copy'].includes(operation)" class="checkbox-label">
        <input v-model="overwrite" type="checkbox" :disabled="busy || blocked" />
        {{ t('overwrite') }}
      </label>
      <p v-if="invalid" class="field-error" role="alert">{{ t(invalid) }}</p>
      <p v-if="error" class="field-error" role="alert">{{ describeError(error) }}</p>
      <p v-if="busy && batch" class="field-help" role="status">
        {{ t('batchProgress', { done: progress, total: entries.length }) }}
      </p>
      <footer class="dialog-actions">
        <button class="button text" type="button" :disabled="busy" @click="emit('close')">
          {{ blocked ? t('close') : t('cancel') }}
        </button>
        <button
          v-if="!blocked"
          class="button"
          :class="operation === 'delete' ? 'danger' : 'filled'"
          type="submit"
          :disabled="busy"
        >
          <span v-if="busy" class="spinner small" aria-hidden="true"></span>
          {{ busy ? t('working') : operation === 'delete' ? t('delete') : t('confirm') }}
        </button>
      </footer>
    </form>
  </BaseDialog>
</template>
