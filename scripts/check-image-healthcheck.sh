#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Pat Ramsey
#
# Fails unless every platform image in an OCI layout tarball carries a HEALTHCHECK in its
# config. It reads the config blobs themselves, not `docker image inspect`: that goes through
# a container engine, and some (Podman's Docker-compatible API among them) do not report a
# healthcheck that the image does carry. A HEALTHCHECK is easy to lose quietly when the
# image's format or build settings change, and the image has no curl to make up for it.
#
# Usage: scripts/check-image-healthcheck.sh IMAGE.tar
#   docker buildx build --output type=oci,dest=IMAGE.tar ...
set -euo pipefail

tar_file=${1:?usage: check-image-healthcheck.sh IMAGE.tar}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
tar -xf "$tar_file" -C "$dir"

blob() { echo "$dir/blobs/${1/://}"; }

checked=0
# Walk the index; descend into nested indexes. Attestation manifests (provenance, SBOM) are
# not images and have no platform worth checking.
check_index() {
  local index=$1 digest media
  while read -r digest media; do
    case $media in
      *image.index* | *manifest.list*) check_index "$(blob "$digest")" ;;
      *) check_manifest "$(blob "$digest")" "$digest" ;;
    esac
  done < <(jq -r '.manifests[]
    | select((.annotations["vnd.docker.reference.type"] // "") != "attestation-manifest")
    | "\(.digest) \(.mediaType)"' "$index")
}
check_manifest() {
  local config
  config=$(jq -r '.config.digest' "$1")
  if ! jq -e '.config.Healthcheck.Test | length > 0' "$(blob "$config")" >/dev/null; then
    echo "image $2 has no HEALTHCHECK in its config" >&2
    exit 1
  fi
  checked=$((checked + 1))
}
check_index "$dir/index.json"

if [ "$checked" -eq 0 ]; then
  echo "no images found in $tar_file" >&2
  exit 1
fi
echo "ok   $checked image(s) carry a HEALTHCHECK"
