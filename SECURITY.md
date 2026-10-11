# Security policy

## Reporting a vulnerability

Please **do not open a public issue** for a security problem. Report it privately through GitHub:
[Security, then Report a vulnerability](https://github.com/hookinlet/hookinlet/security/advisories/new) on this
repository. Include the version (`hookinlet version`), how you run it, and the smallest steps that show the
problem. Do not include real tokens, signing secrets, or webhook payloads you would not post publicly.

You will get an acknowledgement, and an assessment, as soon as the maintainer can give one. Fixes are released
with a note in `CHANGELOG.md` and, where it matters, a GitHub security advisory crediting you if you want to be
credited.

## What is in scope

hookinlet is a server that stores what strangers send it and hands it to programs that act on it, so the
interesting problems are these:

- getting past authentication or authorization: reading an inbox without its token, using the admin API
  without the admin token, learning that another inbox exists from a credential for a different one;
- accepting a webhook whose signature does not verify, or storing unverified content as verified;
- leaking a secret: the admin token, an agent token, a verifier secret, the master key, or a token hash,
  in a response, a log line, an error, a backup, or a file with the wrong permissions;
- ways to make the server crash, hang, or use unbounded memory or disk with a small request;
- content from a sender that can forge the structure of what an agent reads (an event stream, a page, an MCP
  result);
- mistakes in the release artifacts: a binary or image that does not match its checksum or its source.

## What is not

- An agent that does what a message tells it to. Message content is untrusted input written by whoever sent the
  webhook, and the documentation says so; defending an agent against it is the agent's job. A way for content to
  *look* verified when it is not is in scope.
- Running more than one instance on one data directory, or exposing the server without TLS: both are documented
  as unsupported.
- Reports that need the admin token, the master key, or write access to the data directory to begin with.
- Denial of service by sheer volume of traffic. Put a rate limit in front, or in a `Hooks` implementation.

## Supported versions

hookinlet is pre-1.0 and only the latest release is supported.

## Where to look

[`GUARDRAILS.md`](GUARDRAILS.md) lists the security rules the code is held to (rules 16 to 26), and
[`docs/deploying.md`](docs/deploying.md) says how to run it safely.
