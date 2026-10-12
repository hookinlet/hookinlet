<script setup lang="ts">
// A small set of line icons, drawn inline so nothing is fetched.
defineProps<{ name: 'overview' | 'inboxes' | 'tokens' | 'copy' | 'check' | 'sun' | 'moon' | 'monitor' | 'plus' | 'logout' | 'alert' }>()
</script>

<template>
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <template v-if="name === 'overview'">
      <rect x="3" y="3" width="7" height="9" rx="1.5" /><rect x="14" y="3" width="7" height="5" rx="1.5" />
      <rect x="14" y="12" width="7" height="9" rx="1.5" /><rect x="3" y="16" width="7" height="5" rx="1.5" />
    </template>
    <path v-else-if="name === 'inboxes'" d="M4 13l2-8h12l2 8M4 13v6h16v-6M4 13h5l1 2h4l1-2h5" />
    <template v-else-if="name === 'tokens'"><circle cx="8" cy="15" r="4" /><path d="M11 12l9-9M16 7l3 3" /></template>
    <template v-else-if="name === 'copy'"><rect x="9" y="9" width="11" height="11" rx="2" /><path d="M5 15V6a2 2 0 0 1 2-2h9" /></template>
    <path v-else-if="name === 'check'" d="M5 12.5l4.5 4.5L19 7.5" />
    <template v-else-if="name === 'sun'"><circle cx="12" cy="12" r="4" /><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" /></template>
    <path v-else-if="name === 'moon'" d="M20 14.5A8 8 0 0 1 9.5 4 8 8 0 1 0 20 14.5z" />
    <template v-else-if="name === 'monitor'"><rect x="3" y="4" width="18" height="12" rx="2" /><path d="M8 20h8M12 16v4" /></template>
    <path v-else-if="name === 'plus'" d="M12 5v14M5 12h14" />
    <path v-else-if="name === 'logout'" d="M15 4h3a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-3M10 8l-4 4 4 4M6 12h10" />
    <template v-else-if="name === 'alert'"><path d="M12 3l9.5 17h-19z" /><path d="M12 10v4M12 17.5v.5" /></template>
  </svg>
</template>
