#!/usr/bin/env bash
set -euo pipefail

repository=${GITHUB_REPOSITORY,,}
primary=${2:-ghcr.io/$repository}
sha_tag=sha-$GITHUB_SHA
mirror=${3:-}
if [[ -z "$mirror" && -n ${MIRROR_USER:-} ]]; then
  mirror=docker.io/${MIRROR_USER,,}/${repository##*/}
fi

# Only a registry "not found" response permits creation. Never overwrite a tag
# after a transient connection, authorization, or server failure.
published_digest() {
  local error_file digest
  error_file=$(mktemp)
  if digest=$(regctl image digest "$1" 2>"$error_file"); then
    rm -f "$error_file"
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
    printf '%s\n' "$digest"
  elif grep -Eiq 'manifest unknown|name unknown|http 404|response status: 404' "$error_file"; then
    rm -f "$error_file"
  else
    cat "$error_file" >&2
    rm -f "$error_file"
    return 1
  fi
}

# Read from the immutable digest for every check, including reruns which
# reuse a previous build rather than downloading this run's artifacts.
verify_index() {
  local source=$1 repo=${1%@*} index arch image attestation statement
  index=$(regctl manifest get "$source" --format raw-body)
  jq -e '.schemaVersion == 2 and (.manifests | length) == 4' <<< "$index" >/dev/null
  for arch in amd64 arm64; do
    image=$(jq -er --arg arch "$arch" '[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch)] |
      select(length == 1) | .[0].digest' <<< "$index")
    regctl image config "$repo@$image" | jq -e --arg arch "$arch" --arg rev "$GITHUB_SHA" \
      --arg src "${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY" '
      .os == "linux" and .architecture == $arch and .config.Labels["org.opencontainers.image.revision"] == $rev
      and .config.Labels["org.opencontainers.image.source"] == $src' >/dev/null
    attestation=$(jq -er --arg image "$image" '[.manifests[] |
      select(.annotations["vnd.docker.reference.type"] == "attestation-manifest" and
             .annotations["vnd.docker.reference.digest"] == $image)] | select(length == 1) | .[0].digest' <<< "$index")
    statement=$(regctl manifest get "$repo@$attestation" --format raw-body | jq -er '
      [.layers[] | select((.annotations["in-toto.io/predicate-type"] // "") | startswith("https://slsa.dev/provenance/"))] |
      select(length == 1) | .[0].digest')
    regctl blob get "$repo" "$statement" | jq -e --arg sha "${image#sha256:}" '
      (.predicateType | startswith("https://slsa.dev/provenance/")) and any(.subject[]; .digest.sha256 == $sha)' >/dev/null
  done
}

# Validate before making any registry writes. Distinct Git tags must stay distinct.
release_tag=
if [[ "$GITHUB_REF" == refs/tags/v* ]]; then
  release_tag=${GITHUB_REF#refs/tags/}
  if ! [[ "$release_tag" =~ ^v[A-Za-z0-9_.-]{0,127}$ ]]; then
    echo "Release name must already be a valid Docker tag (at most 128 ASCII characters)" >&2
    exit 1
  fi
fi

# The publication job serializes checks and writes across SHAs and release tags.
case "$1" in
  lookup)
    digest=$(published_digest "$primary:$sha_tag")
    echo "digest=$digest" >> "$GITHUB_OUTPUT"
    ;;
  prepare)
    # Sign only the index assembled from this workflow run's tested archives.
    layout="ocidir://$RUNNER_TEMP/tested-layout"
    for arch in amd64 arm64; do
      regctl image import "$layout:$arch" "$RUNNER_TEMP/tested/image-$arch.tar"
      regctl manifest get "$layout:$arch" --format raw-body > "$RUNNER_TEMP/$arch.json"
    done
    # Keep the original descriptors, including BuildKit's attestation annotations.
    jq -s '{schemaVersion: 2, mediaType: "application/vnd.oci.image.index.v1+json",
            manifests: [.[].manifests[]]}' "$RUNNER_TEMP/amd64.json" "$RUNNER_TEMP/arm64.json" |
      regctl manifest put --content-type application/vnd.oci.image.index.v1+json "$layout:tested"
    digest=$(regctl image digest "$layout:tested")
    verify_index "$layout@$digest"
    echo "digest=$digest" >> "$GITHUB_OUTPUT"
    echo "name=$primary" >> "$GITHUB_OUTPUT"
    ;;
  publish)
    digest=$(published_digest "$primary:$sha_tag")
    if [[ -z "$digest" ]]; then
      digest=${PREPARED_DIGEST:?missing attested digest for the tested archives}
      [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
      layout="ocidir://$RUNNER_TEMP/tested-layout"
      [[ "$(regctl image digest "$layout:tested")" == "$digest" ]]
      source="$layout@$digest"
    else
      source="$primary@$digest"
      gh attestation verify "oci://$source" --repo "$GITHUB_REPOSITORY" \
        --signer-workflow "$GITHUB_REPOSITORY/.github/workflows/verify.yml" \
        --source-digest "$GITHUB_SHA" --deny-self-hosted-runners
      verify_index "$source"
      echo "Reusing attested $sha_tag at $digest"
    fi
    destinations=("$primary:$sha_tag")
    if [[ -n "$mirror" ]]; then destinations+=("$mirror:$sha_tag"); fi
    if [[ -n "$release_tag" ]]; then
      destinations+=("$primary:$release_tag")
      if [[ -n "$mirror" ]]; then destinations+=("$mirror:$release_tag"); fi
    fi
    missing=()
    for destination in "${destinations[@]}"; do
      existing=$(published_digest "$destination")
      if [[ -z "$existing" ]]; then
        missing+=("$destination")
      elif [[ "$existing" != "$digest" ]]; then
        echo "Refusing to replace $destination at $existing with $digest" >&2
        exit 1
      fi
    done
    for destination in "${missing[@]}"; do
      regctl image copy "$source" "$destination"
    done
    echo "digest=$digest" >> "$GITHUB_OUTPUT"
    ;;
  latest)
    # Called under the shared latest lock. Check again inside every retry.
    head_sha=$(gh api "repos/$GITHUB_REPOSITORY/git/ref/heads/main" --jq .object.sha)
    if [[ "$head_sha" != "$GITHUB_SHA" ]]; then
      echo "Skipping $GITHUB_SHA; main is now $head_sha"
      exit 0
    fi
    [[ "$IMAGE_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]]
    [[ "$(regctl image digest "$primary:$sha_tag")" == "$IMAGE_DIGEST" ]]
    regctl image copy "$primary@$IMAGE_DIGEST" "$primary:latest"
    if [[ -n "$mirror" ]]; then regctl image copy "$mirror@$IMAGE_DIGEST" "$mirror:latest"; fi
    ;;
  *) exit 2 ;;
esac
