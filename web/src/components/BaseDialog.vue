<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, useId } from 'vue'
import { X } from '@lucide/vue'
import { t } from '../lib/i18n'

const props = withDefaults(defineProps<{ title: string; busy?: boolean }>(), { busy: false })
const emit = defineEmits<{ close: [] }>()
const dialog = ref<HTMLDialogElement>()
const titleID = useId()
let previousFocus: HTMLElement | null = null
function requestClose(): void {
  if (!props.busy) emit('close')
}
function trapFocus(event: KeyboardEvent): void {
  if (event.key !== 'Tab' || !dialog.value?.open) return
  const openDialogs = document.querySelectorAll('dialog[open]')
  if (openDialogs[openDialogs.length - 1] !== dialog.value) return
  const elements = Array.from(
    dialog.value.querySelectorAll<HTMLElement>(
      'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), summary, [tabindex]:not([tabindex="-1"])',
    ),
  ).filter((element) => element.getClientRects().length > 0)
  const index = elements.indexOf(document.activeElement as HTMLElement)
  if (!elements.length) {
    event.preventDefault()
    dialog.value.focus()
    return
  }
  if (event.shiftKey && index <= 0) {
    event.preventDefault()
    elements[elements.length - 1]?.focus()
  } else if (!event.shiftKey && (index === -1 || index === elements.length - 1)) {
    event.preventDefault()
    elements[0]?.focus()
  }
}
function backdrop(event: MouseEvent): void {
  if (event.target !== dialog.value) return
  const bounds = dialog.value.getBoundingClientRect()
  if (
    event.clientX < bounds.left ||
    event.clientX > bounds.right ||
    event.clientY < bounds.top ||
    event.clientY > bounds.bottom
  )
    requestClose()
}
onMounted(() => {
  previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
  dialog.value?.showModal()
  dialog.value?.querySelector<HTMLElement>('[autofocus]')?.focus()
  document.addEventListener('keydown', trapFocus, true)
})
onBeforeUnmount(() => {
  document.removeEventListener('keydown', trapFocus, true)
  dialog.value?.close()
  if (previousFocus?.isConnected) previousFocus.focus({ preventScroll: true })
})
</script>

<template>
  <dialog
    ref="dialog"
    class="modal"
    :aria-labelledby="titleID"
    :aria-busy="busy"
    @cancel.prevent="requestClose"
    @click="backdrop"
  >
    <header class="modal-head">
      <h2 :id="titleID">{{ title }}</h2>
      <button class="icon-button" :aria-label="t('close')" :disabled="busy" @click="requestClose">
        <X :size="22" aria-hidden="true" />
      </button>
    </header>
    <div class="modal-body"><slot /></div>
  </dialog>
</template>
