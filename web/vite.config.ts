import vue from '@vitejs/plugin-vue'
import { mkdirSync, writeFileSync } from 'node:fs'
import { defineConfig } from 'vitest/config'

// bundledPackages records which npm packages ended up in the build, so that the licence
// file lists what the binary carries and not everything the lockfile can reach.
function bundledPackages() {
  return {
    name: 'bundled-packages',
    generateBundle(_: unknown, bundle: Record<string, { modules?: Record<string, unknown> }>) {
      const names = new Set<string>()
      for (const chunk of Object.values(bundle)) {
        for (const id of Object.keys(chunk.modules ?? {})) {
          const m = /\/node_modules\/((?:@[^/]+\/)?[^/]+)\//.exec(id.replaceAll('\\', '/'))
          if (m?.[1]) names.add(m[1])
        }
      }
      mkdirSync('node_modules/.cache', { recursive: true })
      writeFileSync('node_modules/.cache/bundled-packages.json', JSON.stringify([...names].sort()))
    },
  }
}

// The console is served by hookinlet itself at /admin/, so every asset path starts
// there, and the build goes straight into the directory the Go binary embeds.
export default defineConfig({
  base: '/admin/',
  plugins: [vue(), bundledPackages()],
  build: {
    outDir: '../internal/ui/dist', // relative to this directory
    emptyOutDir: true,
    sourcemap: false,
    // Everything is a file the page loads from its own origin: the content security
    // policy allows no inline script or style.
    assetsInlineLimit: 0,
    cssCodeSplit: false,
    // One chunk, so there is nothing to preload, and the polyfill would only add code.
    modulePreload: { polyfill: false },
  },
  server: {
    // `npm run dev` proxies the API to a server on :8080.
    proxy: { '/v1': 'http://localhost:8080', '/healthz': 'http://localhost:8080', '/readyz': 'http://localhost:8080' },
  },
  test: { environment: 'node' },
})
