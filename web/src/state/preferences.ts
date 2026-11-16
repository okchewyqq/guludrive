import { computed, ref, watch, watchEffect } from 'vue'
import { messageLanguage } from '../lib/i18n.ts'

export type Language = 'system' | 'zh' | 'en'
export type Theme = 'system' | 'light' | 'dark'
function read(key: string): string | null {
  try { return localStorage.getItem(key) } catch { return null }
}
function save(key: string, value: string): void {
  try { localStorage.setItem(key, value) } catch { /* In-memory preferences still work. */ }
}
const savedLanguage = read('guludrive.language')
export const language = ref<Language>(
  savedLanguage === 'zh' || savedLanguage === 'en' ? savedLanguage : 'system',
)
const browserLanguage = ref(navigator.language)
window.addEventListener('languagechange', () => { browserLanguage.value = navigator.language })
export const effectiveLanguage = computed(() => language.value === 'system'
  ? (browserLanguage.value.toLowerCase().startsWith('zh') ? 'zh' : 'en')
  : language.value,
)
watch(effectiveLanguage, value => { messageLanguage.value = value }, { immediate: true })
const savedTheme = read('guludrive.theme')
export const theme = ref<Theme>(
  savedTheme === 'dark' || savedTheme === 'light' ? savedTheme : 'system',
)
const media = window.matchMedia('(prefers-color-scheme: dark)')
const systemDark = ref(media.matches)
media.addEventListener('change', (event) => { systemDark.value = event.matches })
watch(language, value => save('guludrive.language', value))
watch(theme, value => save('guludrive.theme', value))
watchEffect(() => {
  document.documentElement.lang = effectiveLanguage.value === 'zh' ? 'zh-CN' : 'en'
  const actualTheme = theme.value === 'system' ? (systemDark.value ? 'dark' : 'light') : theme.value
  document.documentElement.dataset.theme = actualTheme
  document.querySelector('meta[name="theme-color"]')
    ?.setAttribute('content', actualTheme === 'dark' ? '#17231b' : '#eaf1eb')
})
