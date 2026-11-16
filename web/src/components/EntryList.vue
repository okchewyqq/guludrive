// Native-style file rows (ui/view.go fileRow): no checkboxes, single click
// toggles selection, double click opens a folder.
<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { ChevronRight, Container, File, Folder, Image, Music, Video } from '@lucide/vue'
import type { Entry } from '../api/models'
import { entryKind, formatDate, formatSize, t } from '../lib/i18n'

const props = defineProps<{
  entries: Entry[]
  selected: Set<string>
  disabled?: boolean
}>()
const emit = defineEmits<{
  toggle: [entry: Entry]
  open: [entry: Entry]
}>()
// Mirror the native 1040 px breakpoint that adds the SIZE/MODIFIED/TYPE columns.
const wideQuery = window.matchMedia('(min-width: 1040px)')
const wide = ref(wideQuery.matches)
const onWideChange = (event: MediaQueryListEvent): void => {
  wide.value = event.matches
}
wideQuery.addEventListener('change', onWideChange)
onBeforeUnmount(() => wideQuery.removeEventListener('change', onWideChange))
const rows = computed(() => props.entries.map((entry) => ({ entry, kind: entryKind(entry) })))
function tile(entry: Entry) {
  if (entry.isDir) return entry.path.replace(/^\/+|\/+$/g, '').includes('/') ? Folder : Container
  if (entry.contentType.startsWith('image/')) return Image
  if (entry.contentType.startsWith('video/')) return Video
  if (entry.contentType.startsWith('audio/')) return Music
  return File
}
function subtitle(kind: string, entry: Entry): string {
  const parts = [kind]
  if (!entry.isDir) parts.push(formatSize(entry.size))
  parts.push(formatDate(entry.modTime))
  return parts.join(' · ')
}
</script>
<template>
  <div v-if="wide" class="entry-head" aria-hidden="true">
    <span class="entry-head-name">{{ t('colName') }}</span>
    <span class="col-size">{{ t('colSize') }}</span>
    <span class="col-modified">{{ t('colModified') }}</span>
    <span class="col-type">{{ t('colType') }}</span>
    <span class="entry-chevron-spacer"></span>
  </div>
  <ul class="entry-list" :aria-label="t('files')">
    <li v-for="{ entry, kind } in rows" :key="entry.id">
      <button
        type="button"
        class="entry-row"
        :class="{ selected: selected.has(entry.path) }"
        :disabled="disabled"
        :aria-pressed="selected.has(entry.path)"
        :title="entry.name"
        @click="emit('toggle', entry)"
        @dblclick="entry.isDir && !disabled && emit('open', entry)"
      >
        <span class="entry-tile"><component :is="tile(entry)" :size="21" aria-hidden="true" /></span>
        <span class="entry-body">
          <span class="entry-name">{{ entry.name }}</span>
          <span v-if="!wide" class="entry-meta">{{ subtitle(kind, entry) }}</span>
        </span>
        <template v-if="wide">
          <span class="col-size">{{ entry.isDir ? '—' : formatSize(entry.size) }}</span>
          <span class="col-modified">{{ formatDate(entry.modTime) }}</span>
          <span class="col-type" :title="kind">{{ kind }}</span>
        </template>
        <ChevronRight v-if="entry.isDir" class="entry-chevron" :size="18" aria-hidden="true" />
        <span v-else class="entry-chevron-spacer"></span>
      </button>
    </li>
  </ul>
</template>
