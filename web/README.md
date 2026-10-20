# The admin console

A Vue 3 + TypeScript + Vite single-page app. hookinlet embeds the built files and serves them at `/admin`, next to
the admin API.

The built files are committed to `../internal/ui/dist`, so you do not need Node to build or install the Go
program. CI rebuilds the files from `package-lock.json`. It fails if they differ from what is committed.

```sh
cd web
npm ci --ignore-scripts      # exact versions from the lockfile; no install scripts
npm run dev                  # http://localhost:5173/admin/, proxying the API to :8080
npm run types                # regenerate src/api/schema.d.ts from ../docs/openapi.json
npm test                     # unit tests
npm run build                # writes ../internal/ui/dist; commit the result
npm run e2e:install          # once: the browser the end-to-end tests use
npm run e2e                  # builds the Go binary, then runs the browser tests against it
SCREENSHOTS=1 npm run e2e -- e2e/screenshots.spec.ts   # regenerates ../docs/images
node licenses.mjs            # after a dependency change; then run ../scripts/third-party-licenses.sh
```

Or run `make web` from the repository root. It does all of this.

The project holds this app to these rules:

- The app never renders sender-written text as HTML. It uses no `v-html` and no `innerHTML`. CI fails on both.
- The app has no inline script or style. It loads nothing from another origin. The content security policy
  (`internal/ui/ui.go`) blocks both.
- The admin token stays in memory. The app never puts it in a URL or writes it to storage.
- `vue` is the only runtime dependency. A new one needs a reason.
