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

Everyone taking part is expected to follow the [code of conduct](CODE_OF_CONDUCT.md).

No outside code is being merged for now. Please open an issue to propose a change first. If the maintainer invites
a pull request, you will be asked to sign a contributor license agreement before it can be merged (its text will be
published before any invitation is sent), so that contributed code can ship in both the open-source and the
commercial builds of the project.

## How the project works

The conventions below are the ones the code already follows. They matter if you are invited to contribute, and
they are useful if you are only reading.

- **Seams.** Handlers reach storage, notification, auth, and secrets only through the interfaces in `pkg/inbox`,
  never through a type assertion to a concrete implementation (the documented optional capabilities, such as
  `Backupper`, are the exception). Every interface has a conformance suite in `pkg/inboxtest`, and a behaviour that
  is in no suite is not part of the contract. A new capability is a new optional interface, not a new method on an
  old one ([stability](docs/stability.md)).
- **Data integrity.** Nothing returns 2xx from ingest until the message is committed. The body is stored byte for
  byte and signatures are verified against those raw bytes before any parsing. Cursors are per-inbox sequence
  numbers assigned in commit order, never timestamps or global row ids. In the SQLite store every write goes
  through the single writer connection. Migrations are append-only: a shipped one is never edited.
- **Security.** Never log request bodies, verifier secrets, tokens, or redacted headers (log sequence numbers,
  slugs, sizes, statuses, durations). Compare secrets, tokens, and signatures only with `crypto/subtle` or
  `hmac.Equal`. Tokens are stored only as hashes and shown once; verifier secrets are stored only sealed by a
  `SecretBox`; admin responses never contain either. A credential for one inbox must not reveal whether another
  exists (404, not 403). Body limits are enforced with `http.MaxBytesReader` before reading.
- **Runtime.** Startup makes no network calls and reaches "listening" fast; slow work (retention, vacuum) runs after.
  Streaming handlers exit promptly on client disconnect and on shutdown. Single-node assumptions in the default
  implementations are commented where they matter.
- **Errors.** Error responses are JSON, `{"error": {"code": "…", "message": "…"}}`.
- **Open core.** This repository is complete and useful by itself. Nothing in it is crippled to sell something
  else, and there is no telemetry, license check, or hosting-specific code. Cloud-only concepts (organizations,
  users, plans, billing) do not belong in `pkg/`.
- **Stdlib first.** A new dependency needs a one-line justification, a license compatible with AGPL-3.0, and
  `CGO_ENABLED=0` must keep building.
- **Tests come first, and prove themselves.** `go test -race ./...` and `make lint` pass before every commit. A bug fix comes with a test that fails without it. A behaviour that
  matters is checked by breaking it on purpose and seeing a test fail. A verifier change includes negative tests
  (wrong secret, tampered body, stale timestamp, malformed header).
- **Docs move with the code.** A change to a request or response shape updates `docs/api.md`, `docs/openapi.json`
  (a test compares it with the server), and `docs/mcp.md` if a tool is affected. After editing
  the document, run `make generate`: `internal/api` is generated from it, CI fails if it is stale, and the
  server refuses requests that break it. To add an operation, add it to the document and run `make generate`;
  the server will not build until a type of `pkg/server/*_api.go` implements it, and nothing else is needed to
  make the route exist, require its credential, or check its parameters. Examples in the documentation are
  run by `scripts/test-docs.sh`.
- **Public Go API.** Everything under `pkg/` is pre-1.0 and may change; every change to it is called out in
  `CHANGELOG.md`. After v1.0.0, semantic versioning applies.

## License and file headers

See [`LICENSING.md`](LICENSING.md) for what the license means for people using the project.

hookinlet is licensed under the GNU Affero General Public License, version 3 only (`AGPL-3.0-only`). Every Go
source file starts with:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey
```

The example clients in `examples/` (`curl`, `python`, `go-agent` apart from its test, and `relay`) are MIT licensed
instead (see `examples/LICENSE`) and carry `// SPDX-License-Identifier: MIT` with `Copyright (c)`.

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

A pull request runs the fast checks: build, vet, unit tests (without the race detector), lint, and the static
checks on the OpenAPI document and the generated code. The race detector, the documentation examples, the image
build and smoke test, and the release dry run run when a change lands on `main` and every night, so run the ones
you touched yourself before asking for review.

Commit messages say what changed and why, for whoever reads the history later.
