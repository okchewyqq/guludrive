<script setup lang="ts">
import { Folder } from '@lucide/vue'
defineProps<{ title: string; description?: string }>()
</script>
<template>
  <div class="empty-state">
    <span class="empty-icon"><Folder :size="28" aria-hidden="true" /></span>
    <h2>{{ title }}</h2>
    <p v-if="description">{{ description }}</p>
    <slot />
  </div>
</template>
