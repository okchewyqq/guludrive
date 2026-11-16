// Clipboard contents are never persisted. Manual fallback is DOM-only and short lived.
let manualDialog: HTMLDialogElement | null = null
export function clearClipboardFallback(): void {
  if (!manualDialog) return
  const field = manualDialog.querySelector('textarea')
  if (field) field.value = ''
  manualDialog.remove()
  manualDialog = null
}
function showManualCopy(value: string, labels: { title: string; close: string }): void {
  clearClipboardFallback()
  const dialog = document.createElement('dialog')
  dialog.className = 'manual-copy-dialog'
  const heading = document.createElement('h2')
  heading.id = 'manual-copy-title'
  heading.textContent = labels.title
  dialog.setAttribute('aria-labelledby', heading.id)
  const field = document.createElement('textarea')
  field.value = value
  field.readOnly = true
  field.spellcheck = false
  field.setAttribute('aria-label', labels.title)
  const close = document.createElement('button')
  close.type = 'button'
  close.className = 'button outlined'
  close.textContent = labels.close
  const previous = document.activeElement
  const dismiss = () => {
    clearClipboardFallback()
    if (previous instanceof HTMLElement && previous.isConnected) previous.focus({ preventScroll: true })
  }
  close.addEventListener('click', dismiss)
  dialog.addEventListener('cancel', event => { event.preventDefault(); dismiss() })
  dialog.append(heading, field, close)
  document.body.append(dialog)
  manualDialog = dialog
  dialog.showModal()
  field.focus()
  field.select()
}
export async function copyText(
  value: string,
  field?: HTMLInputElement | HTMLTextAreaElement,
  manual?: { title: string; close: string },
): Promise<boolean> {
  try {
    if (window.isSecureContext && navigator.clipboard) {
      await navigator.clipboard.writeText(value)
      return true
    }
  } catch { /* Try the selected-field fallback on restricted origins. */ }
  const previous = document.activeElement
  const input = field ?? document.createElement('textarea')
  if (!field) {
    input.value = value
    input.setAttribute('readonly', '')
    input.setAttribute('aria-hidden', 'true')
    input.setAttribute('tabindex', '-1')
    input.style.cssText = 'position:fixed;left:-9999px;top:0;opacity:0'
    document.body.append(input)
  }
  let copied = false
  try {
    input.focus({ preventScroll: true })
    input.select()
    copied = document.execCommand('copy')
  } catch { /* The visible manual selection below is the final fallback. */ }
  finally {
    if (!field) {
      input.value = ''
      input.remove()
      if (previous instanceof HTMLElement && previous.isConnected) previous.focus({ preventScroll: true })
    }
  }
  if (!copied && manual) showManualCopy(value, manual)
  return copied
}
