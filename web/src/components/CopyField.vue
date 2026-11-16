<script setup lang="ts">
import { onBeforeUnmount, ref, useId } from 'vue'
import { Check, Copy } from '@lucide/vue'
import { t } from '../lib/i18n'
import { copyText } from '../lib/clipboard'

const props = withDefaults(defineProps<{ label: string; value: string; multiline?: boolean }>(), {
  multiline: false,
})
const input = ref<HTMLInputElement | HTMLTextAreaElement>()
const id = useId()
const feedback = ref<'copied' | 'copyFallback' | null>(null)
let timer: ReturnType<typeof setTimeout> | undefined
let active = true
async function copy(): Promise<void> {
  const copied = await copyText(props.value, input.value)
  if (!active) return
  feedback.value = copied ? 'copied' : 'copyFallback'
  clearTimeout(timer)
  timer = setTimeout(
    () => {
      feedback.value = null
    },
    copied ? 2500 : 7000,
  )
}
onBeforeUnmount(() => {
  active = false
  clearTimeout(timer)
})
</script>

<template>
  <div class="copy-control">
    <label :for="id">{{ label }}</label>
    <div class="copy-box">
      <textarea
        v-if="multiline"
        :id="id"
        ref="input"
        :value="value"
        :rows="Math.min(10, Math.max(3, value.split('\n').length))"
        readonly
        autocomplete="off"
        spellcheck="false"
        @focus="input?.select()"
      ></textarea>
      <input
        v-else
        :id="id"
        ref="input"
        :value="value"
        readonly
        autocomplete="off"
        spellcheck="false"
        @focus="input?.select()"
      />
      <button
        type="button"
        :aria-label="t('copyField', { label })"
        :title="t('copyField', { label })"
        @click="copy"
      >
        <Check v-if="feedback === 'copied'" :size="19" aria-hidden="true" />
        <Copy v-else :size="19" aria-hidden="true" />
      </button>
    </div>
    <span class="copy-feedback" aria-live="polite">{{ feedback ? t(feedback) : '' }}</span>
  </div>
</template>
