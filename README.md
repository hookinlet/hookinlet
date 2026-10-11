# hookinlet

A webhook inbox for AI agents. Point a webhook sender (Stripe, GitHub, Slack, or
anything that can POST) at an inbox; hookinlet verifies the signature, stores the
exact bytes, and lets an agent read them over a REST API, a live event stream, or
MCP tools. One static Go binary, one SQLite file, no other services.

> Status: pre-1.0 and not yet announced. The full README is still to come.

## Run it

```sh
export HOOKINLET_ADMIN_TOKEN=$(openssl rand -base64 32)
export HOOKINLET_MASTER_KEY=$(openssl rand -base64 32)
docker run -d -p 8080:8080 -v hookinlet-data:/data \
  -e HOOKINLET_ADMIN_TOKEN -e HOOKINLET_MASTER_KEY ghcr.io/hookinlet/hookinlet:latest
```

Then follow [the quick start](docs/deploying.md#quick-start).

## Documentation

- [Deploying](docs/deploying.md): what it needs, settings, health checks, upgrades
- [Administration](docs/admin.md): inboxes, retention, backups, the command line
- [API](docs/api.md): ingest, reading, and the live stream
- [MCP](docs/mcp.md): the tools for agents
- [Verifiers](docs/verifiers.md): checking Stripe, GitHub, Slack, and other signatures
- [Extending](docs/extending.md): embedding the server and replacing its parts
- [Releasing](docs/releasing.md): the maintainers' checklist
- [Performance](docs/performance.md): what was measured, and how to repeat it

## Start-up time

With an existing database the server is ready for traffic in milliseconds: the
median of seven restarts (database file already in the operating system's cache),
exec to first successful `/readyz`, on a database of one million messages, was
**4 ms on Linux and 11 ms on macOS**. The project's limit is 500 ms. It makes no network calls at startup, and retention runs after the server
is listening. Repeat it with
`HOOKINLET_BENCH=1 go test -run TestColdStart -v ./cmd/hookinlet/`.

## Licence

AGPL-3.0-only. See `LICENSE` and `NOTICE`.
