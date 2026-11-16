<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { ChevronRight, Copy, FolderOpen, Move, Pencil, Plus, Search, Trash2, Upload, X } from '@lucide/vue'
import BaseDialog from '../components/BaseDialog.vue'
import CopyField from '../components/CopyField.vue'
import EmptyState from '../components/EmptyState.vue'
import EntryList from '../components/EntryList.vue'
import ErrorBanner from '../components/ErrorBanner.vue'
import FileOperationDialog from '../components/FileOperationDialog.vue'
import UploadDialog from '../components/UploadDialog.vue'
import { api } from '../api/client'
import type { Entry, Listing } from '../api/models'
import { copyText } from '../lib/clipboard'
import { createDownloadLinkText, type DownloadLinkText } from '../lib/downloadLinks'
import { entryKind, formatDate, t } from '../lib/i18n'
import type { FileOperation } from '../lib/operations'
import { navigate, route, routeURL } from '../state/navigation'
import { noteError, notify, reportError } from '../state/feedback'
import { refreshStatus } from '../state/session'
import { googleReady, refreshBuckets } from '../state/workspace'
import { uploadRevision, uploadsRunning } from '../state/uploads'

type SortOrder = 'name' | 'newest' | 'size'
const listing = ref<Listing | null>(null)
const loading = ref(false)
const error = ref<unknown>(null)
const search = ref('')
const sort = ref<SortOrder>('name')
// Selection is keyed by path, like the native client, so it survives listing refreshes.
const selected = ref(new Set<string>())
const operation = ref<FileOperation | null>(null)
const operationEntries = ref<Entry[]>([])
const chosenFiles = ref<File[] | null>(null)
const uploadEntries = ref<Entry[]>([])
const preparingUpload = ref(false)
const copyingLink = ref(false)
const downloadLinks = ref<DownloadLinkText | null>(null)
const picker = ref<HTMLInputElement>()
const currentPath = computed(() => route.value.path)
const entries = computed(() => listing.value?.entries ?? [])
const selectedEntries = computed(() =>
  entries.value
    .filter((entry) => selected.value.has(entry.path))
    .sort((a, b) => (a.isDir === b.isDir ? 0 : a.isDir ? -1 : 1)),
)
const single = computed(() => (selectedEntries.value.length === 1 ? selectedEntries.value[0] : null))
const selectedFiles = computed(() => selectedEntries.value.filter((entry) => !entry.isDir))
const title = computed(() => {
  const path = currentPath.value
  return path === '/' ? t('files') : path.slice(path.lastIndexOf('/') + 1)
})
const crumbs = computed(() => [
  { name: t('root'), path: '/', current: currentPath.value === '/' },
  ...currentPath.value
    .split('/')
    .filter(Boolean)
    .map((name, index, parts) => ({
      name,
      path: '/' + parts.slice(0, index + 1).join('/'),
      current: index === parts.length - 1,
    })),
])
const filtered = computed(() => {
  const query = search.value.trim().toLocaleLowerCase()
  return entries.value
    .filter((entry) => entry.name.toLocaleLowerCase().includes(query))
    .sort((a, b) => {
      if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
      const order =
        sort.value === 'name'
          ? a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' })
          : sort.value === 'size'
            ? b.size - a.size
            : new Date(b.modTime).getTime() - new Date(a.modTime).getTime()
      return order
    })
})
const busy = computed(() => loading.value || preparingUpload.value || copyingLink.value)
const canWrite = computed(() => googleReady.value && !busy.value && !uploadsRunning.value)
let generation = 0
let controller: AbortController | null = null
async function load(): Promise<void> {
  const expected = ++generation
  const path = currentPath.value
  controller?.abort()
  controller = new AbortController()
  loading.value = true
  error.value = null
  try {
    const result = await api.list(path, controller.signal)
    if (expected === generation) listing.value = result
  } catch (failure) {
    if (expected === generation && !(failure instanceof DOMException && failure.name === 'AbortError')) {
      error.value = failure
      noteError(failure)
    }
  } finally {
    if (expected === generation) loading.value = false
  }
}
// Native behaviour: any click toggles that row; single click never downloads.
function toggle(entry: Entry): void {
  const next = new Set(selected.value)
  if (next.has(entry.path)) next.delete(entry.path)
  else next.add(entry.path)
  selected.value = next
}
function clearSelection(): void {
  selected.value = new Set()
}
function open(entry: Entry): void {
  if (entry.isDir) navigate('files', entry.path)
}
let linkController: AbortController | null = null
function clearDownloadLink(): void {
  linkController?.abort()
  linkController = null
  downloadLinks.value = null
  copyingLink.value = false
}
async function copyDownloadLink(input: Entry[]): Promise<void> {
  if (busy.value) return
  clearDownloadLink()
  const request = new AbortController()
  linkController = request
  copyingLink.value = true
  try {
    const result = await createDownloadLinkText(
      input,
      (path, signal) => api.createDownloadLink(path, signal),
      request.signal,
    )
    if (!result || request.signal.aborted) return
    const copied = await copyText(result.text)
    if (request.signal.aborted) return
    if (copied) {
      notify(
        result.count === 1 ? 'downloadLinkCopied' : 'downloadLinksCopied',
        result.count === 1 ? undefined : { count: result.count },
      )
    } else downloadLinks.value = result
    void refreshStatus(request.signal).catch((failure: unknown) => {
      if (!request.signal.aborted) reportError(failure)
    })
  } catch (failure) {
    if (!request.signal.aborted) reportError(failure)
  } finally {
    if (linkController === request) copyingLink.value = false
  }
}
function showOperation(value: FileOperation): void {
  operationEntries.value = [...selectedEntries.value]
  operation.value = value
}
async function changed(): Promise<void> {
  await Promise.all([load(), refreshBuckets().catch(reportError)])
}
async function pickFiles(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  input.value = ''
  if (!files.length) return
  const path = currentPath.value
  preparingUpload.value = true
  try {
    const current = await api.list(path)
    if (path !== currentPath.value) return
    listing.value = current
    uploadEntries.value = current.entries
    chosenFiles.value = files
  } catch (failure) {
    reportError(failure)
  } finally {
    preparingUpload.value = false
  }
}
watch(
  currentPath,
  () => {
    clearDownloadLink()
    listing.value = null
    search.value = ''
    operation.value = null
    chosenFiles.value = null
    clearSelection()
    void load()
  },
  { immediate: true },
)
watch(uploadRevision, () => {
  void load()
})
onBeforeUnmount(() => {
  generation++
  controller?.abort()
  clearDownloadLink()
})
</script>

<template>
  <section class="view files-view" :aria-busy="busy">
    <header class="view-heading">
      <h1>{{ title }}</h1>
      <div class="heading-actions">
        <button
          v-if="currentPath === '/'"
          type="button"
          class="button tonal"
          :disabled="!canWrite"
          :title="!googleReady ? t('connectFirst') : undefined"
          @click="showOperation('createBucket')"
        >
          <Plus :size="20" aria-hidden="true" />
          {{ t('newBucket') }}
        </button>
        <button
          v-else
          type="button"
          class="button outline"
          :disabled="!canWrite"
          @click="showOperation('createDirectory')"
        >
          <Plus :size="20" aria-hidden="true" />
          {{ t('newDirectory') }}
        </button>
        <button
          type="button"
          class="button filled"
          :disabled="!canWrite || currentPath === '/'"
          :title="currentPath === '/' ? t('chooseFolder') : undefined"
          @click="picker?.click()"
        >
          <Upload :size="20" aria-hidden="true" />
          {{ t('upload') }}
        </button>
        <input
          ref="picker"
          type="file"
          multiple
          class="sr-only"
          tabindex="-1"
          :aria-label="t('upload')"
          @change="pickFiles"
        />
      </div>
    </header>
    <nav class="crumbs" :aria-label="t('path')">
      <template v-for="(crumb, index) in crumbs" :key="crumb.path">
        <ChevronRight v-if="index > 0" :size="14" aria-hidden="true" />
        <a
          class="button text"
          :href="routeURL('files', crumb.path)"
          :aria-current="crumb.current ? 'page' : undefined"
        ><span>{{ crumb.name }}</span></a>
      </template>
    </nav>
    <div class="file-toolbar">
      <label class="search-box">
        <Search :size="20" aria-hidden="true" />
        <input
          v-model="search"
          type="search"
          :placeholder="t('search')"
          :aria-label="t('findFiles')"
        />
        <button
          v-if="search"
          type="button"
          class="icon-button small"
          :aria-label="t('clearSearch')"
          :title="t('clearSearch')"
          @click="search = ''"
        >
          <X :size="16" aria-hidden="true" />
        </button>
      </label>
      <select v-model="sort" class="sort-select" :aria-label="t('sort')">
        <option value="name">{{ t('sortName') }}</option>
        <option value="newest">{{ t('sortNewest') }}</option>
        <option value="size">{{ t('sortSize') }}</option>
      </select>
    </div>
    <div class="files-card">
      <ErrorBanner v-if="error" :error="error" :busy="loading" @retry="load" />
      <div v-else-if="loading && !listing" class="loading-state" role="status">
        <span class="spinner" aria-hidden="true"></span>
        {{ t('loadingFiles') }}
      </div>
      <EntryList
        v-else-if="filtered.length"
        :entries="filtered"
        :selected="selected"
        :disabled="busy"
        @toggle="toggle"
        @open="open"
      />
      <EmptyState
        v-else-if="search.trim()"
        :title="t('noMatches')"
        :description="t('noMatchesHint')"
      >
        <button type="button" class="button tonal" @click="search = ''">
          {{ t('clearSearch') }}
        </button>
      </EmptyState>
      <EmptyState
        v-else-if="currentPath === '/'"
        :title="t('noBuckets')"
        :description="googleReady ? t('noBucketsHint') : t('connectGoogleHint')"
      >
        <a v-if="!googleReady" class="button filled" :href="routeURL('accounts')">
          {{ t('connectAccount') }}
        </a>
        <button
          v-else
          type="button"
          class="button filled"
          :disabled="!canWrite"
          @click="showOperation('createBucket')"
        >
          <Plus :size="20" aria-hidden="true" />
          {{ t('newBucket') }}
        </button>
      </EmptyState>
      <EmptyState v-else :title="t('emptyFolder')" :description="t('emptyFolderHint')">
        <button type="button" class="button filled" :disabled="!canWrite" @click="picker?.click()">
          <Upload :size="20" aria-hidden="true" />
          {{ t('chooseFiles') }}
        </button>
      </EmptyState>
    </div>
    <div v-if="selectedEntries.length > 1" class="selection-bar">
      <div class="selection-head">
        <span class="selection-name">{{ t('selected', { count: selectedEntries.length }) }}</span>
        <button
          type="button"
          class="icon-button small"
          :aria-label="t('clearSelection')"
          :title="t('clearSelection')"
          @click="clearSelection"
        >
          <X :size="18" aria-hidden="true" />
        </button>
      </div>
      <div class="selection-actions">
        <button
          type="button"
          class="button tonal"
          :disabled="busy || !selectedFiles.length"
          @click="copyDownloadLink(selectedFiles)"
        >
          <Copy :size="20" aria-hidden="true" />
          {{ selectedFiles.length === 1 ? t('copyDownloadLink') : t('copyDownloadLinks') }}
        </button>
        <span v-if="selectedFiles.length !== selectedEntries.length" class="selection-note">
          {{ t('foldersExcluded') }}
        </span>
        <button
          type="button"
          class="button text"
          :disabled="busy || uploadsRunning"
          @click="showOperation('move')"
        >
          <Move :size="20" aria-hidden="true" />
          {{ t('move') }}
        </button>
        <button
          type="button"
          class="button text"
          :disabled="busy || uploadsRunning"
          @click="showOperation('copy')"
        >
          <Copy :size="20" aria-hidden="true" />
          {{ t('copy') }}
        </button>
        <button
          type="button"
          class="button text"
          :disabled="busy || uploadsRunning"
          @click="showOperation('delete')"
        >
          <Trash2 :size="20" aria-hidden="true" />
          {{ t('delete') }}
        </button>
      </div>
    </div>
    <div v-else-if="single" class="selection-bar">
      <div class="selection-head">
        <span class="selection-name" :title="single.name">{{ single.name }}</span>
        <span class="pill muted">{{ entryKind(single) }}</span>
        <button
          type="button"
          class="icon-button small"
          :aria-label="t('clearSelection')"
          :title="t('clearSelection')"
          @click="clearSelection"
        >
          <X :size="18" aria-hidden="true" />
        </button>
      </div>
      <div class="selection-actions">
        <button
          v-if="single.isDir"
          type="button"
          class="button tonal"
          :disabled="busy"
          @click="open(single)"
        >
          <FolderOpen :size="20" aria-hidden="true" />
          {{ t('open') }}
        </button>
        <button
          v-else
          type="button"
          class="button tonal"
          :disabled="busy"
          @click="copyDownloadLink([single])"
        >
          <Copy :size="20" aria-hidden="true" />
          {{ t('copyDownloadLink') }}
        </button>
        <button
          type="button"
          class="button text"
          :disabled="busy || uploadsRunning"
          @click="showOperation('rename')"
        >
          <Pencil :size="20" aria-hidden="true" />
          {{ t('rename') }}
        </button>
        <button
          type="button"
          class="button text"
          :disabled="busy || uploadsRunning"
          @click="showOperation('move')"
        >
          <Move :size="20" aria-hidden="true" />
          {{ t('move') }}
        </button>
        <button
          type="button"
          class="button text"
          :disabled="busy || uploadsRunning"
          @click="showOperation('copy')"
        >
          <Copy :size="20" aria-hidden="true" />
          {{ t('copy') }}
        </button>
        <button
          type="button"
          class="button text"
          :disabled="busy || uploadsRunning"
          @click="showOperation('delete')"
        >
          <Trash2 :size="20" aria-hidden="true" />
          {{ t('delete') }}
        </button>
      </div>
    </div>
    <div v-else class="list-footer">
      <span>{{ t('items', { count: filtered.length }) }}</span>
      <span v-if="!loading">{{ t('footerHint') }}</span>
      <span v-else role="status">{{ t('loading') }}</span>
    </div>
    <BaseDialog
      v-if="downloadLinks"
      :title="t('copyDownloadLink')"
      @close="downloadLinks = null"
    >
      <p class="field-help">{{ t('downloadLinkManual') }}</p>
      <CopyField
        :label="t('copyDownloadLink')"
        :value="downloadLinks.text"
        :multiline="downloadLinks.count > 1"
      />
      <p class="field-help">
        {{ t('downloadLinkExpires', { time: formatDate(downloadLinks.expiresAt) }) }}
      </p>
    </BaseDialog>
    <FileOperationDialog
      v-if="operation"
      :operation="operation"
      :entries="operationEntries"
      :directory="currentPath"
      @close="operation = null"
      @changed="changed"
    />
    <UploadDialog
      v-if="chosenFiles"
      :files="chosenFiles"
      :entries="uploadEntries"
      :path="currentPath"
      @close="chosenFiles = null"
    />
  </section>
</template>
