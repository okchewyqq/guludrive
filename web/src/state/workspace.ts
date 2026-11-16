import { computed, ref } from 'vue'
import { api } from '../api/client.ts'
import type { Account, Bucket } from '../api/models'
import { status } from './session.ts'

export const accounts = ref<Account[]>([])
export const buckets = ref<Bucket[]>([])
// A bucket keeps its bound account, independently of the default for new buckets.
export const bucketAccounts = computed(() => {
  const emails = new Map(accounts.value.map(account => [account.id, account.email]))
  return new Map(buckets.value.map(bucket => [bucket.name, emails.get(bucket.album.accountID) ?? '']))
})
export const workspaceLoading = ref(false)
export const workspaceError = ref<unknown>(null)
export const googleReady = computed(() => accounts.value.some((account) => account.selected))
let generation = 0
export function clearWorkspace(): void {
  generation++
  accounts.value = []
  buckets.value = []
  workspaceError.value = null
  workspaceLoading.value = false
}
export async function refreshBuckets(): Promise<void> {
  const expected = generation
  const result = await api.buckets()
  if (expected === generation) buckets.value = result.buckets
}
export async function refreshWorkspace(): Promise<void> {
  const expected = ++generation
  workspaceLoading.value = true
  workspaceError.value = null
  try {
    const results = await Promise.allSettled([api.accounts(), api.buckets(), api.status()])
    if (expected !== generation) return
    const [accountResult, bucketResult, currentStatus] = results
    if (accountResult.status === 'fulfilled') accounts.value = accountResult.value.accounts
    if (bucketResult.status === 'fulfilled') buckets.value = bucketResult.value.buckets
    if (currentStatus.status === 'fulfilled') status.value = currentStatus.value
    const failure = results.find((result) => result.status === 'rejected')
    if (failure?.status === 'rejected') throw failure.reason
  } catch (error) {
    if (expected === generation) workspaceError.value = error
    throw error
  } finally {
    if (expected === generation) workspaceLoading.value = false
  }
}
