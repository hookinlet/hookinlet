# Contributing to hookinlet

Thank you for looking. This page says what is welcome and how to take part.

## Issues and discussions are welcome

Bug reports, questions, and ideas are welcome as issues. Please use the templates: they ask for what makes a
problem reproducible.

- **For a bug**, tell us the version (`hookinlet version`), how you run it (binary or image, and behind which
  proxy), and the smallest set of steps that shows the problem. Never paste tokens, signing secrets, or webhook
  payloads you would not post in public.
- **For a security problem**, do not open an issue; see [SECURITY.md](SECURITY.md).
- **For an idea**, start with what you are trying to do, then say how you would like it done. `docs/roadmap.md`
  lists what we already have in mind and where it belongs.

## Pull requests are by invitation

Everyone taking part is expected to follow the [code of conduct](CODE_OF_CONDUCT.md).

We are not merging outside code for now. Please open an issue to propose a change first. If the maintainer invites
you to send a pull request, you will need to sign a contributor license agreement before it can be merged. That
lets your code ship in both the open-source and the commercial builds of the project. We will publish the text of
the agreement before we send any invitation.

## How the project works

These are the rules the code already follows. They matter if you are invited to contribute. They are also useful
if you are only reading.

- **Seams.** Handlers reach storage, notification, auth, and secrets only through the interfaces in `pkg/inbox`.
  They never use a type assertion to reach a concrete implementation. The documented optional capabilities, such as
  `Backupper`, are the exception. Every interface has a conformance suite in `pkg/inboxtest`. A behaviour that no
  suite covers is not part of the contract. To add a capability, add a new optional interface instead of a new
  method on an old one ([stability](docs/stability.md)).
- **Data integrity.** Ingest returns 2xx only after the message is committed. The body is stored byte for byte, and
  signatures are checked against those raw bytes before anything is parsed. Cursors are per-inbox sequence numbers,
  assigned in commit order. They are never timestamps or global row ids. In the SQLite store, every write goes
  through the single writer connection. Migrations are append-only. Once a migration ships, never edit it.
- **Security.** Never log request bodies, verifier secrets, tokens, or redacted headers. Log sequence numbers,
  slugs, sizes, statuses, and durations instead. Compare secrets, tokens, and signatures only with `crypto/subtle`
  or `hmac.Equal`. Tokens are stored only as hashes and shown once. Verifier secrets are stored only sealed by a
  `SecretBox`. Admin responses never contain either. A credential for one inbox must not reveal whether another
  inbox exists (return 404, not 403). Enforce body limits with `http.MaxBytesReader` before reading.
- **Runtime.** Startup makes no network calls and reaches "listening" fast. Slow work (retention, vacuum) runs
  after that. Streaming handlers exit promptly when the client disconnects and when the server shuts down. Where the
  default implementations assume a single node, a comment says so.
- **Errors.** Error responses are JSON, `{"error": {"code": "…", "message": "…"}}`.
- **Open core.** This repository is complete and useful by itself. Nothing in it is held back to sell something
  else. It has no telemetry, license check, or hosting-specific code. Concepts that only a hosted version needs
  (organizations, users, plans, billing) do not belong in `pkg/`.
- **Stdlib first.** Prefer Go's standard library. A new dependency needs a one-line reason and a license that works
  with AGPL-3.0. The build must keep working with `CGO_ENABLED=0`.
- **Tests come first, and prove themselves.** `go test -race ./...` and `make lint` must pass before every commit.
  A bug fix comes with a test that fails without the fix. To check a behaviour that matters, break it on purpose and
  see a test fail. A verifier change includes negative tests (wrong secret, tampered body, stale timestamp,
  malformed header).
- **Docs move with the code.** If you change a request or response shape, update `docs/api.md` and
  `docs/openapi.json` (a test compares it with the server). Update `docs/mcp.md` too if a tool is affected. After
  you edit the document, run `make generate`. `internal/api` is generated from it, CI fails if it is stale, and the
  server refuses requests that break it. To add an operation, add it to the document and run `make generate`. The
  server will not build until a type in `pkg/server/*_api.go` implements it. Nothing else is needed to make the
  route exist, require its credential, or check its parameters. `scripts/test-docs.sh` runs the examples in the
  documentation, and `go test ./docs` checks that every relative link and `#anchor` works.
- **Public Go API.** Everything under `pkg/` is pre-1.0 and may change. `CHANGELOG.md` lists every change to it.
  After v1.0.0, semantic versioning applies.

## License and file headers

See [`LICENSING.md`](LICENSING.md) for what the license means if you use the project.

hookinlet uses the GNU Affero General Public License, version 3 only (`AGPL-3.0-only`). Every Go source file starts
with:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey
```

The example clients in `examples/` (`curl`, `python`, `go-agent` apart from its test, and `relay`) use the MIT
license instead (see `examples/LICENSE`). They carry `// SPDX-License-Identifier: MIT` with `Copyright (c)`.

Every dependency needs a license that works with the AGPL.

## Development

```sh
make test   # go test -race ./...
make lint   # golangci-lint run ./...
make run    # serve with throwaway development credentials
```

Before a change is ready, run `make lint test`. If you touched the documentation or the examples, also run
`scripts/test-docs.sh`. Go is pinned in `go.mod`. Build the Docker image with `make docker` and run its smoke test
with `scripts/smoke-docker.sh hookinlet:dev`. `scripts/soak` runs a built binary under a mixed workload for half an
hour and checks that nothing it acknowledged is lost (`go run -tags soak ./scripts/soak -h`). It sits behind a build
tag, so `go build ./...` and `go test ./...` skip it.

A pull request runs the fast checks. These are the build, vet, unit tests (without the race detector), lint, the
static checks on the OpenAPI document and the generated code, and the admin console's build, unit tests and
end-to-end tests in a browser. The slower checks are the race detector, the documentation examples, the image build
and smoke test, and the release dry run. They run every night. They also run when a maintainer starts CI by hand,
and when a change lands on `main` that touches anything they read. A change that only edits the changelog or
another project document does not start them. A release needs a full run to have passed. So run the slower checks
that cover your change yourself before you ask for review.

Write each commit message to say what changed and why, for whoever reads the history later.
