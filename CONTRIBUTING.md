# Contributing to hookinlet

## Issues and discussions are welcome

Bug reports, questions, and ideas are welcome as issues and discussions. For
bugs, include the version (`hookinlet version`), how you run it, and the
smallest steps that reproduce the problem. Never paste tokens, signing
secrets, or webhook payloads you would not post publicly.

## Pull requests are by invitation

No outside code is being merged for now. Please open an issue to propose a
change first. If the maintainer invites a pull request, you will be asked to
sign a contributor license agreement before it can be merged, so that
contributed code can ship in both the open-source and the commercial builds.

## License and file headers

hookinlet is licensed under the GNU Affero General Public License, version 3
only (`AGPL-3.0-only`). Every Go source file starts with:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey
```

`make lint` enforces this. Dependencies must have AGPL-compatible licenses.

## Development

```sh
make test   # go test -race ./...
make lint   # golangci-lint run ./...
make run    # serve with throwaway development credentials
```

Go is pinned in `go.mod`. The project is stdlib-first; a new dependency needs
a one-line justification in the pull request.
