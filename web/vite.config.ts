import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

// The console is served by hookinlet itself at /admin/, so every asset path starts
// there, and the build goes straight into the directory the Go binary embeds.
export default defineConfig({
  base: '/admin/',
  plugins: [vue()],
  build: {
    outDir: '../internal/ui/dist', // relative to this directory
    emptyOutDir: true,
    sourcemap: false,
    // Everything is a file the page loads from its own origin: the content security
    // policy allows no inline script or style.
    assetsInlineLimit: 0,
    cssCodeSplit: false,
  },
  server: {
    // `npm run dev` proxies the API to a server on :8080.
    proxy: { '/v1': 'http://localhost:8080', '/healthz': 'http://localhost:8080', '/readyz': 'http://localhost:8080' },
  },
  test: { environment: 'node' },
})
