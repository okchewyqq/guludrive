import { ref } from 'vue'

type View = 'files' | 'accounts' | 'settings'
function readRoute(): { view: View; path: string } {
  const [name, query = ''] = location.hash.slice(1).split('?')
  const view = name === '/accounts' ? 'accounts' : name === '/settings' ? 'settings' : 'files'
  const path = new URLSearchParams(query).get('path') || '/'
  return { view, path: path.startsWith('/') ? path : '/' }
}
export const route = ref(readRoute())
window.addEventListener('hashchange', () => {
  route.value = readRoute()
})
export function routeURL(view: View, path = '/'): string {
  return (
    '#/' +
    view +
    (view === 'files' && path !== '/' ? '?' + new URLSearchParams({ path }).toString() : '')
  )
}
export function navigate(view: View, path = '/'): void {
  location.hash = routeURL(view, path)
}
