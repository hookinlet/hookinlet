# Releasing

For whoever tags a release. The work is in `.github/workflows/release.yml` and
`.goreleaser.yaml`; this page is the checklist around it.

## Before tagging

1. `main` is green, and `CHANGELOG.md` has the release's entry at the top (move "Unreleased" into a
   heading for the version). The workflow will not publish a tag whose commit is not on `main` or
   has no passing CI run, so there is nothing to bypass: wait for CI.
2. Run the workflow by hand (Actions, Release, Run workflow). It builds the
   archives, checksums, SBOMs, and both image architectures, and smoke-tests the
   image, but publishes nothing, whatever ref you run it from.
3. Optionally try it locally: `goreleaser release --snapshot --clean` (needs
   `syft` on the PATH for the SBOMs).

## Tagging

A tag is the release. Use a plain version: `v1.2.3`, or `v1.2.3-rc.1` for a
release candidate. Anything else (`vnext`, `v1.2.3+build`) is refused before
anything is built.

```sh
git tag -a v0.1.0-rc.1 -m "v0.1.0-rc.1"
git push origin v0.1.0-rc.1
```

The workflow then:

1. checks that the tagged commit is on `main` and that CI passed on it, runs `govulncheck`, checks the
   tag, and works out the image tags;
2. builds and publishes the archives (each with `LICENSE`, `NOTICE` and `THIRD_PARTY_LICENSES`),
   `checksums.txt`, and one SBOM per archive to a GitHub release, and attests their build
   provenance (marked a pre-release when the tag has a hyphen);
3. only if that succeeded, builds the image, smoke-tests it, and publishes
   `linux/amd64` and `linux/arm64` to `ghcr.io/hookinlet/hookinlet` with provenance
   and an SBOM.

A pre-release image gets only its own version tag. A final release also moves
`latest` (if it is the highest final version) and `MAJOR.MINOR` (if it is the
highest in that line), so tagging a patch to an old line moves neither `latest`
nor a newer line's tag.

Promoting a candidate is a new tag, `v0.1.0`, on the same commit: the workflow
builds exactly the tag that was pushed, not whichever tag sorts first.

## Checking what you downloaded

```sh
sha256sum -c --ignore-missing checksums.txt
gh attestation verify hookinlet_0.1.0_linux_amd64.tar.gz --repo hookinlet/hookinlet
docker buildx imagetools inspect ghcr.io/hookinlet/hookinlet:0.1.0   # provenance and SBOM are attached
```

## Keeping it current

`THIRD_PARTY_LICENSES` is generated (`make licenses`) and CI fails if it is out of date, or if a
dependency has a license other than MIT, BSD, ISC or Apache-2.0. Dependabot opens a weekly pull
request for Go modules, the GitHub Actions (pinned by commit) and the Docker base images
(pinned by digest); `govulncheck` runs on every change and every night. Turn on Dependabot security updates in the
repository settings (Code security) as well, or only the weekly pull requests are opened.
A grouped Go-module pull request changes versions only; it passes CI unless it adds a dependency or
changes a license, in which case run `make licenses` on its branch.

## The first release

Things the first release taught, kept here because each is a setting or a step only a maintainer can do.

1. **Cut a release candidate first** (`v0.1.0-rc.1`). It exercises everything a final tag does (archives,
   attestation, image push) and, being a pre-release, moves neither `latest` nor `MAJOR.MINOR`. Promote it
   with a final tag on the same commit.
2. **The repository settings** the release depends on: Actions limited to GitHub-owned actions plus
   `docker/*`, `golangci/*`, `goreleaser/*` and `anchore/*`, with pinning by commit required; a ruleset on
   `main` (pull request and CI checks required, no force-push); a ruleset on `v*` tags (administrators
   only, no deletion); secret scanning with push protection; private vulnerability reporting, which
   `SECURITY.md` and the issue chooser send reporters to.
3. **The GHCR package** is created by the first image push, as private. The organization must allow public
   packages (organization settings, Packages) before the package's visibility can be changed. Make it public,
   check that it is linked to this repository, and check with an anonymous
   `docker pull ghcr.io/hookinlet/hookinlet:<version>`. A package that already exists under the same name,
   linked to another repository, refuses the push with `permission_denied: read_package`: delete it or give
   this repository write access to it under "Manage Actions access".
4. **The provenance attestation** is made only while the repository is public.

## After

- Check the image with an anonymous `docker pull` (a new package starts private; see above).
- `docker buildx imagetools inspect ghcr.io/hookinlet/hookinlet:<version>` should
  list both platforms and the attestations.
- Download one archive, run `sha256sum -c --ignore-missing checksums.txt`, and run
  `./hookinlet version`.

## Keeping the pins current

Everything the release depends on is pinned so that it cannot change under it:
the actions by commit, the GoReleaser version by number, and the two base images
in the `Dockerfile` by digest. To update an image:

```sh
docker buildx imagetools inspect golang:1.27.2 --format '{{.Manifest.Digest}}'
docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot --format '{{.Manifest.Digest}}'
```

put the digests after the tags in the `FROM` lines, and run
`scripts/smoke-docker.sh` on a build of the result.

## What is not done

The macOS binaries are not signed or notarized. The archives and `checksums.txt` carry a
signed build-provenance attestation (`gh attestation verify`), which says that this
repository's release workflow built them; it is made only when the repository is public (GitHub
does not offer it to a private one on a free plan), so a release cut while private has
checksums and SBOMs only, which guard against a damaged download and not against a
compromised release. The container image has BuildKit provenance and an SBOM attached, which are
not signed.
