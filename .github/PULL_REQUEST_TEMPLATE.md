<!-- Pull requests are by invitation; see CONTRIBUTING.md. -->

## What and why

## How it was checked

- [ ] `make lint test` passes
- [ ] A bug fix has a test that fails without it
- [ ] `scripts/test-docs.sh` passes, if documentation or examples changed
- [ ] `docs/api.md`, `docs/openapi.json`, and `docs/mcp.md` match any change to a request or response
- [ ] Changes to `pkg/` are in `CHANGELOG.md`
- [ ] New dependencies are justified, AGPL-compatible, and build with `CGO_ENABLED=0`
- [ ] Nothing here logs a body, token, or secret
