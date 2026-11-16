import { ref } from 'vue'
import { advanceSessionGeneration, api, ApiError, onUnauthorized, sessionGeneration } from '../api/client.ts'
import type { Status } from '../api/models'
import { reportError } from './feedback.ts'

export const initialized = ref<boolean | null>(null)
export const authenticated = ref(false)
export const restoring = ref(true)
export const restoreError = ref<unknown>(null)
export const status = ref<Status | null>(null)
export const loginUsername = ref('')
let restoreRevision = 0
// Local revocation never sends DELETE /api/session.
export function clearSession(username = ''): void {
  advanceSessionGeneration()
  authenticated.value = false
  status.value = null
  loginUsername.value = username
  restoreError.value = null
}
onUnauthorized(() => clearSession())
export async function refreshStatus(signal?: AbortSignal): Promise<void> {
  const expected = sessionGeneration()
  const result = await api.status(signal)
  if (expected === sessionGeneration() && authenticated.value && !signal?.aborted) status.value = result
}
export async function restoreSession(): Promise<void> {
  const revision = ++restoreRevision
  const expected = sessionGeneration()
  const current = () => revision === restoreRevision && expected === sessionGeneration()
  restoring.value = true
  restoreError.value = null
  try {
    const setup = await api.setupStatus()
    if (!current()) return
    initialized.value = setup.initialized
    if (!setup.initialized) {
      authenticated.value = false
      return
    }
    const result = await api.status()
    if (!current()) return
    status.value = result
    authenticated.value = true
  } catch (error) {
    if (!current()) return
    if (initialized.value && error instanceof ApiError && error.failure.code) {
      authenticated.value = true
      reportError(error)
    } else if (!(error instanceof ApiError && error.status === 401)) restoreError.value = error
  } finally {
    if (revision === restoreRevision) restoring.value = false
  }
}
export async function signIn(username: string, password: string, signal?: AbortSignal): Promise<void> {
  const expected = sessionGeneration()
  const result = await api.login(username, password, signal)
  if (!result || signal?.aborted || expected + 1 !== sessionGeneration()) return
  initialized.value = true
  loginUsername.value = username
  authenticated.value = true
}
export async function initializeAdministrator(username: string, password: string, signal?: AbortSignal): Promise<void> {
  const expected = sessionGeneration()
  try {
    const result = await api.setup(username, password, signal)
    if (signal?.aborted || expected !== sessionGeneration()) return
    if (result.initialized !== true) throw new ApiError(502)
    initialized.value = true
    loginUsername.value = username
    await signIn(username, password, signal)
  } catch (error) {
    if (signal?.aborted || expected !== sessionGeneration()) throw error
    if (error instanceof ApiError && error.status === 409) {
      // Another client won initialization. Recheck, never overwrite or retry creation.
      await restoreSession()
      return
    }
    throw error
  }
}
export async function signOut(): Promise<void> {
  const expected = sessionGeneration()
  await api.logout()
  if (expected === sessionGeneration()) clearSession()
}
