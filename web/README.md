# The admin console

A Vue 3 + TypeScript + Vite single-page app. hookinlet embeds the built files and serves
them at `/admin`, beside the admin API.

The built files are committed to `../internal/ui/dist`, so building or installing the Go
program needs no Node. CI rebuilds them from `package-lock.json` and fails if they differ
from what is committed.

```sh
cd web
npm ci --ignore-scripts      # exact versions from the lockfile; no install scripts
npm run dev                  # http://localhost:5173/admin/, proxying the API to :8080
npm run types                # regenerate src/api/schema.d.ts from ../docs/openapi.json
npm test                     # unit tests
npm run build                # writes ../internal/ui/dist; commit the result
node licenses.mjs            # after a dependency change; then run ../scripts/third-party-licenses.sh
```

Or `make web` from the repository root, which does all of it.

Rules the project holds this app to:

- Nothing a sender wrote is ever rendered as HTML: no `v-html`, no `innerHTML`.
  CI fails on both.
- No inline script or style, and nothing loaded from another origin: the content security
  policy (`internal/ui/ui.go`) allows neither.
- The admin token stays in memory. It is never put in a URL or written to storage.
- Runtime dependencies are `vue` alone. A new one needs a reason.
