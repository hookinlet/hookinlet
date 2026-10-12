<script setup lang="ts">
import { computed } from 'vue'

// An activity line: an area under a stroke, with the last point marked. The scale runs
// from zero to the largest value, so a flat line really is nothing happening.
const props = withDefaults(defineProps<{ values: number[]; width?: number; height?: number }>(), { width: 110, height: 28 })

const geometry = computed(() => {
  const { values, width, height } = props
  const pad = 3
  const max = Math.max(1, ...values)
  const stepX = values.length > 1 ? (width - pad * 2) / (values.length - 1) : 0
  const pts = values.map((v, i) => [pad + i * stepX, height - pad - (v / max) * (height - pad * 2)] as const)
  const line = pts.map(([x, y], i) => `${i === 0 ? 'M' : 'L'}${x.toFixed(1)} ${y.toFixed(1)}`).join(' ')
  const last = pts[pts.length - 1]
  const first = pts[0]
  const area = last && first ? `${line} L${last[0].toFixed(1)} ${height} L${first[0].toFixed(1)} ${height}Z` : ''
  return { line, area, last, empty: values.every((v) => v === 0) }
})
</script>

<template>
  <svg :viewBox="`0 0 ${width} ${height}`" :width="width" :height="height" role="img" :aria-label="`Activity over the last 24 hours: ${values.reduce((a, b) => a + b, 0)} messages`">
    <template v-if="geometry.empty">
      <path :d="`M3 ${height - 3} H${width - 3}`" class="flat" />
    </template>
    <template v-else>
      <path :d="geometry.area" class="area" />
      <path :d="geometry.line" class="stroke" />
      <circle v-if="geometry.last" :cx="geometry.last[0]" :cy="geometry.last[1]" r="2.4" class="dot" />
    </template>
  </svg>
</template>

<style scoped>
.flat { stroke: var(--line); stroke-width: 1.6; fill: none; }
.area { fill: var(--accent-soft); }
.stroke { stroke: var(--accent); stroke-width: 1.6; fill: none; stroke-linejoin: round; stroke-linecap: round; }
.dot { fill: var(--accent); }
</style>
