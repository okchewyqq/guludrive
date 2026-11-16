<template>
  <span class="brand">
    <img :src="icon" alt="" width="42" height="42" />
    <span>GuluDrive</span>
  </span>
</template>
<script setup lang="ts">
const icon = import.meta.env.BASE_URL + 'icon.svg'
</script>
