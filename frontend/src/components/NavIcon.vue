<script setup>
defineProps({ name: { type: String, required: true } });
</script>

<template>
  <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4"
    stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <template v-if="name === 'news'">
      <path d="M3 2.5h10v11H3zM5.5 5h5M5.5 7.5h5M5.5 10h3"/>
    </template>
    <template v-else-if="name === 'checks'">
      <path d="M8 10.5a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5ZM8 10.5V13M2.5 8H1M15 8h-1.5M3.7 3.7 2.6 2.6M12.3 3.7l1.1-1.1"/>
    </template>
    <template v-else-if="name === 'secrets'">
      <circle cx="6.5" cy="8" r="3"/>
      <path d="M8.5 6.5 14 12M12.5 9.5l1.5 1.5"/>
    </template>
    <template v-else>
      <rect x="2" y="2" width="5" height="5" rx="1"/>
      <rect x="9" y="2" width="5" height="5" rx="1"/>
      <rect x="2" y="9" width="5" height="5" rx="1"/>
      <rect x="9" y="9" width="5" height="5" rx="1"/>
    </template>
  </svg>
</template>