<script setup lang="ts">
import { Container, Folder, HardDrive, Settings, Users } from '@lucide/vue'
import { t } from '../lib/i18n'
import { joinPath } from '../lib/paths'
import { route, routeURL } from '../state/navigation'
import { buckets, bucketAccounts } from '../state/workspace'

defineEmits<{ navigate: [] }>()
</script>
<template>
  <div class="nav-content">
    <nav :aria-label="t('menu')" class="main-navigation">
      <a
        :href="routeURL('files')"
        :class="{ active: route.view === 'files' }"
        :aria-current="route.view === 'files' ? 'page' : undefined"
        @click="$emit('navigate')"
      >
        <HardDrive :size="21" aria-hidden="true" />
        {{ t('files') }}
      </a>
      <a
        :href="routeURL('accounts')"
        :class="{ active: route.view === 'accounts' }"
        :aria-current="route.view === 'accounts' ? 'page' : undefined"
        @click="$emit('navigate')"
      >
        <Users :size="21" aria-hidden="true" />
        {{ t('accounts') }}
      </a>
      <a
        :href="routeURL('settings')"
        :class="{ active: route.view === 'settings' }"
        :aria-current="route.view === 'settings' ? 'page' : undefined"
        @click="$emit('navigate')"
      >
        <Settings :size="21" aria-hidden="true" />
        {{ t('settings') }}
      </a>
    </nav>
    <div class="sidebar-divider"></div>
    <div class="sidebar-section-label">
      {{ t('buckets') }}
      <span>{{ buckets.length }}</span>
    </div>
    <nav class="bucket-navigation" :aria-label="t('buckets')">
      <a
        :href="routeURL('files')"
        :class="{ active: route.view === 'files' && route.path === '/' }"
        :aria-current="route.view === 'files' && route.path === '/' ? 'page' : undefined"
        @click="$emit('navigate')"
      >
        <Container :size="18" aria-hidden="true" />
        <span class="bucket-label"><span>{{ t('root') }}</span></span>
      </a>
      <a
        v-for="bucket in buckets"
        :key="bucket.id"
        :href="routeURL('files', joinPath('/', bucket.name))"
        :class="{
          active:
            route.view === 'files' &&
            (route.path === '/' + bucket.name || route.path.startsWith('/' + bucket.name + '/')),
        }"
        @click="$emit('navigate')"
      >
        <Folder :size="18" aria-hidden="true" />
        <span class="bucket-label">
          <span :title="bucket.name">{{ bucket.name }}</span>
          <small :title="bucketAccounts.get(bucket.name)">{{ bucketAccounts.get(bucket.name) }}</small>
        </span>
      </a>
    </nav>
  </div>
</template>
