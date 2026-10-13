# Licensing

In plain words. The legal terms are in [`LICENSE`](LICENSE) (and [`examples/LICENSE`](examples/LICENSE)), and
they win if anything here differs. This page is a summary, not legal advice.

## The short version

hookinlet is licensed under the **GNU Affero General Public License, version 3 only** (`AGPL-3.0-only`). The
example clients in [`examples/`](examples) that only talk to hookinlet over HTTP are **MIT** licensed, so you can
copy them into your own programs.

## What needs nothing from you

- **Running hookinlet as it is**, for any purpose, in a company or at home, including as a service your own
  products and agents use. The AGPL asks nothing of you for running an unmodified copy.
- **Using its API from your own code, in any language,** whatever your code's licence. Your program is a client of a
  separate service and is not covered by the AGPL.
- **Copying the example clients** (`examples/curl`, `python`, `go-agent`, `relay`) into your own programs. Keep the MIT
  notice in `examples/LICENSE` with them.
- **Reading, building, and modifying the code** for your own use.

## When the AGPL applies

| You | What the AGPL asks |
|---|---|
| Modify hookinlet and let other people use your version over a network | Offer those users the source of your modified version, under the AGPL. |
| Give copies of hookinlet, modified or not, to others | Give them the source too, under the AGPL, with the license text. |
| Import its Go packages (`pkg/server`, `pkg/inbox`, and the rest) into your own program and give that program to others, or let others use it over a network | Your program, as a whole, is covered by the AGPL: offer its source to those users under the AGPL. |
| Do any of the above and cannot or do not want to follow the AGPL | You need a commercial license (below). |

If you only call the HTTP API, the last three rows do not apply to you. If your lawyers are unsure, run hookinlet as a
separate service and use its API.

## A commercial license

Some companies cannot use AGPL software, whatever it asks of them, and some want to embed the Go packages in a
product they keep closed. For those cases a commercial license is available: write to
**licensing@hookinlet.com** and say what you want to do. Terms are agreed case by case. It is possible because
the project has one copyright holder, and contributions come in under a contributor license agreement (see
[`CONTRIBUTING.md`](CONTRIBUTING.md)) so that this stays true.

## What the license does not cover

The license covers the code. It does not give you the right to use the name hookinlet or its logo in a way that
suggests your product is the project or is endorsed by it.

## Third-party code

What is inside the binary has its own licenses, all permissive (MIT, BSD, ISC, Apache-2.0, and the SIL OFL for
the console's fonts). They are listed in [`THIRD_PARTY_LICENSES`](THIRD_PARTY_LICENSES), which ships with every
release.
