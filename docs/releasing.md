# Releasing

For whoever tags a release. The work is in `.github/workflows/release.yml` and
`.goreleaser.yaml`; this page is the checklist around it.

## Before tagging

1. `main` is green, and `CHANGELOG.md` has what changed under "Unreleased".
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

1. checks the tag and works out the image tags;
2. builds and publishes the archives, `checksums.txt`, and one SBOM per archive to
   a GitHub release (marked a pre-release when the tag has a hyphen);
3. only if that succeeded, builds the image, smoke-tests it, and publishes
   `linux/amd64` and `linux/arm64` to `ghcr.io/hookinlet/hookinlet` with provenance
   and an SBOM.

A pre-release image gets only its own version tag. A final release also moves
`latest` (if it is the highest final version) and `MAJOR.MINOR` (if it is the
highest in that line), so tagging a patch to an old line moves neither `latest`
nor a newer line's tag.

Promoting a candidate is a new tag, `v0.1.0`, on the same commit: the workflow
builds exactly the tag that was pushed, not whichever tag sorts first.

## After

- A new GHCR package starts private. Make it public in the repository's package
  settings before the quick start in `docs/deploying.md` can be followed by anyone
  else, and check with an anonymous `docker pull`.
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

The macOS binaries are not signed or notarized, and the archives and checksums are
not signed. The checksums and SBOMs come from the same workflow run as the
archives, so they guard against a damaged download, not against a compromised
release.
