# Contributing to hookinlet

Thank you for looking. This page says what is welcome and how to go about it.

## Issues and discussions are welcome

Bug reports, questions, and ideas are welcome as issues. Please use the templates: they ask for what makes a
problem reproducible.

- **For a bug**, include the version (`hookinlet version`), how you run it (binary or image, behind which
  proxy), and the smallest steps that show it. Never paste tokens, signing secrets, or webhook payloads you would
  not post publicly.
- **For a security problem**, do not open an issue; see [SECURITY.md](SECURITY.md).
- **For an idea**, say what you are trying to do before how you would like it done. `docs/roadmap.md` lists what
  is already considered and where it belongs.

## Pull requests are by invitation

No outside code is being merged for now. Please open an issue to propose a change first. If the maintainer invites
a pull request, you will be asked to sign a contributor license agreement before it can be merged, so that
contributed code can ship in both the open-source and the commercial builds.

## How the project works

The conventions below are the ones the code already follows. They matter if you are invited to contribute, and
they are useful if you are only reading.

- **The rules are in [`GUARDRAILS.md`](GUARDRAILS.md).** Read it first. The ones people trip over: handlers reach
  storage, notification, auth, and secrets only through the interfaces in `pkg/inbox`; every interface has a
  conformance suite in `pkg/inboxtest`; nothing returns 2xx from ingest before the message is committed; bodies are
  stored byte for byte and verified before any parsing; cursors are per-inbox sequence numbers and never
  timestamps; and never log bodies, tokens, or secrets.
- **Open core.** This repository is complete and useful by itself. Nothing in it is crippled to sell something
  else, and there is no telemetry, license check, or hosting-specific code. Cloud-only concepts (organizations,
  users, plans, billing) do not belong in `pkg/`.
- **Stdlib first.** A new dependency needs a one-line justification, a license compatible with AGPL-3.0, and
  `CGO_ENABLED=0` must keep building.
- **Tests come first, and prove themselves.** A bug fix comes with a test that fails without it. A behaviour that
  matters is checked by breaking it on purpose and seeing a test fail. A verifier change includes negative tests
  (wrong secret, tampered body, stale timestamp, malformed header).
- **Docs move with the code.** A change to a request or response shape updates `docs/api.md`, `docs/openapi.json`
  (a test compares it with the server), and `docs/mcp.md` if a tool is affected. After editing
  the document, run `make generate`: `internal/api` is generated from it, CI fails if it is stale, and the
  server refuses requests that break it. Examples in the documentation are
  run by `scripts/test-docs.sh`.
- **Public Go API.** Everything under `pkg/` is pre-1.0 and may change; every change to it is called out in
  `CHANGELOG.md`. After v1.0.0, semantic versioning applies.
- **Work comes in phases.** `PLAN.md` describes them and what each must prove before the next begins.

## License and file headers

hookinlet is licensed under the GNU Affero General Public License, version 3 only (`AGPL-3.0-only`). Every Go
source file starts with:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey
```

Dependencies must have AGPL-compatible licenses.

## Development

```sh
make test   # go test -race ./...
make lint   # golangci-lint run ./...
make run    # serve with throwaway development credentials
```

Before a change is ready, run `make lint test`, and `scripts/test-docs.sh` if you touched the documentation or
the examples. Go is pinned in `go.mod`. The Docker image and its smoke test are `make docker` and
`scripts/smoke-docker.sh hookinlet:dev`.

Commit messages say what changed and why, for whoever reads the history later.
