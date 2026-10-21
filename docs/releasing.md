# Releasing

This page is for whoever tags a release. The work happens in `.github/workflows/release.yml` and
`.goreleaser.yaml`. This page is the checklist around it.

## Before tagging

1. `main` is green, and `CHANGELOG.md` has the release's entry at the top. Move "Unreleased" into a heading for
   the version.
2. Start the full CI on `main` (Actions, CI, Run workflow). A merge to `main` runs the heavy checks only if it
   changed something they read, to save CI minutes. The heavy checks are the race detector, the documentation
   examples, the image build and the release dry run. They also run every night, and a manual run always runs
   them all. The release workflow will not publish a tag unless its commit is on `main` and has a passing nightly
   or manual run. You cannot bypass this, so wait for the run.
3. Run the workflow by hand (Actions, Release, Run workflow). It builds the archives, checksums, SBOMs, and both
   image architectures, and it smoke-tests the image. It publishes nothing, whatever ref you run it from.
4. Optionally, try it locally with `goreleaser release --snapshot --clean`. It needs `syft` on the PATH for the
   SBOMs.

## Tagging

A tag is the release. Use a plain version: `v1.2.3`, or `v1.2.3-rc.1` for a release candidate. The workflow
refuses anything else (`vnext`, `v1.2.3+build`) before it builds anything.

```sh
git tag -a v0.1.0-rc.1 -m "v0.1.0-rc.1"
git push origin v0.1.0-rc.1
```

The workflow then:

1. checks that the tagged commit is on `main` and that CI passed on it, runs `govulncheck`, checks the tag, and
   works out the image tags;
2. builds the archives (each with `LICENSE`, `NOTICE` and `THIRD_PARTY_LICENSES`), `checksums.txt`, and one SBOM
   per archive, and publishes them to a GitHub release. It also attests their build provenance. It marks the
   release as a pre-release when the tag has a hyphen;
3. only if step 2 succeeded, builds the image, smoke-tests it, and publishes `linux/amd64` and `linux/arm64` to
   `ghcr.io/hookinlet/hookinlet` with provenance and an SBOM.

A pre-release image gets only its own version tag. A final release also moves `latest` if it is the highest final
version. It moves `MAJOR.MINOR` if it is the highest in that line. So a patch tag on an old line moves neither
`latest` nor a newer line's tag.

To promote a candidate, push a new tag, `v0.1.0`, on the same commit. The workflow builds exactly the tag you
pushed, not whichever tag sorts first.

## Checking what you downloaded

```sh
sha256sum -c --ignore-missing checksums.txt
gh attestation verify hookinlet_0.1.0_linux_amd64.tar.gz --repo hookinlet/hookinlet
docker buildx imagetools inspect ghcr.io/hookinlet/hookinlet:0.1.0   # provenance and SBOM are attached
```

## Keeping it current

`make licenses` generates `THIRD_PARTY_LICENSES`. CI fails if the file is out of date, or if a dependency has a
license other than MIT, BSD, ISC or Apache-2.0. Dependabot opens a weekly pull request for Go modules, the GitHub
Actions (pinned by commit) and the Docker base images (pinned by digest). `govulncheck` runs on every change and
every night. Also turn on Dependabot security updates in the repository settings (Code security). Without them,
Dependabot opens only the weekly pull requests.

A grouped Go-module pull request changes versions only. It passes CI unless it adds a dependency or changes a
license. In that case, run `make licenses` on its branch.

## The first release

The first release taught us these things. Each is a setting or a step that only a maintainer can do.

1. **Cut a release candidate first** (`v0.1.0-rc.1`). It does everything a final tag does (archives,
   attestation, image push). Because it is a pre-release, it moves neither `latest` nor `MAJOR.MINOR`. Promote it
   with a final tag on the same commit.
2. **Set up the repository settings** the release depends on:
   - Limit Actions to GitHub-owned actions plus `docker/*`, `golangci/*`, `goreleaser/*` and `anchore/*`. Require
     pinning by commit.
   - Add a ruleset on `main`: require pull requests and CI checks, and allow no force-push.
   - Add a ruleset on `v*` tags: administrators only, no deletion.
   - Turn on secret scanning with push protection.
   - Turn on private vulnerability reporting. `SECURITY.md` and the issue chooser send reporters to it.
3. **The GHCR package** is created by the first image push, as private. Before you can change its visibility, the
   organization must allow public packages (organization settings, Packages). Make the package public. Check that
   it is linked to this repository. Then check with an anonymous `docker pull ghcr.io/hookinlet/hookinlet:<version>`.
   A package may already exist under the same name, linked to another repository. It refuses the push with
   `permission_denied: read_package`. Delete it, or give this repository write access to it under "Manage Actions
   access".
4. **The provenance attestation** is made only while the repository is public.

## After

- Check the image with an anonymous `docker pull` (a new package starts private; see above).
- `docker buildx imagetools inspect ghcr.io/hookinlet/hookinlet:<version>` should
  list both platforms and the attestations.
- Download one archive, run `sha256sum -c --ignore-missing checksums.txt`, and run
  `./hookinlet version`.

## Keeping the pins current

Everything the release depends on is pinned, so it cannot change under you. The actions are pinned by commit, the
GoReleaser version by number, and the two base images in the `Dockerfile` by digest. To update an image:

```sh
docker buildx imagetools inspect golang:1.27.2 --format '{{.Manifest.Digest}}'
docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot --format '{{.Manifest.Digest}}'
```

Then put the digests after the tags in the `FROM` lines. Build the result and run `scripts/smoke-docker.sh` on it.

## What is not done

The macOS binaries are not signed or notarized.

The archives and `checksums.txt` carry a signed build-provenance attestation (`gh attestation verify`). It says
that this repository's release workflow built them. It is made only when the repository is public, because GitHub
does not offer it to a private repository on a free plan. A release cut while the repository is private has
checksums and SBOMs only. They guard against a damaged download, but not against a compromised release.

The container image has BuildKit provenance and an SBOM attached. Neither is signed.
