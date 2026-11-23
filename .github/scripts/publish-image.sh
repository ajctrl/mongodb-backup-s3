#!/usr/bin/env bash
set -euo pipefail

repository=${GITHUB_REPOSITORY,,}
primary=${2:-ghcr.io/$repository}
sha_tag=sha-$GITHUB_SHA
mirror=
if [[ -n ${MIRROR_USER:-} ]]; then
  mirror=docker.io/${MIRROR_USER,,}/${repository##*/}
fi

# Only a registry "not found" response permits creation. Never overwrite a tag
# after a transient connection, authorization, or server failure.
published_digest() {
  local error_file digest
  error_file=$(mktemp)
  if digest=$(regctl image digest "$primary:$sha_tag" 2>"$error_file"); then
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

# The workflow serializes writers to a SHA tag, including release-tag runs.
case "$1" in
  lookup)
    digest=$(published_digest)
    echo "digest=$digest" >> "$GITHUB_OUTPUT"
    ;;
  publish)
    digest=$(published_digest)
    if [[ -z "$digest" ]]; then
      layout="ocidir://$RUNNER_TEMP/tested-layout"
      for arch in amd64 arm64; do
        regctl image import "$layout:$arch" "$RUNNER_TEMP/tested/image-$arch.tar"
        regctl manifest get "$layout:$arch" --format raw-body > "$RUNNER_TEMP/$arch.json"
        jq -e --arg arch "$arch" '
          [.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch) | .digest] as $images |
          [.manifests[] | select(.annotations["vnd.docker.reference.type"] == "attestation-manifest")
            | .annotations["vnd.docker.reference.digest"]] as $subjects |
          ($images | length) == 1 and $subjects == $images and (.manifests | length) == 2
        ' "$RUNNER_TEMP/$arch.json" >/dev/null
      done
      # Keep the original descriptors, including BuildKit's attestation annotations.
      jq -s '{schemaVersion: 2, mediaType: "application/vnd.oci.image.index.v1+json",
              manifests: [.[].manifests[]]}' "$RUNNER_TEMP/amd64.json" "$RUNNER_TEMP/arm64.json" |
        regctl manifest put --content-type application/vnd.oci.image.index.v1+json "$layout:tested"
      digest=$(regctl image digest "$layout:tested")
      regctl image copy "$layout@$digest" "$primary:$sha_tag"
    else
      echo "Reusing $sha_tag at $digest"
    fi
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
    if [[ -n "$mirror" ]]; then
      regctl image copy "$primary@$digest" "$mirror:$sha_tag"
    fi
    if [[ "$GITHUB_REF" == refs/tags/v* ]]; then
      # Preserve the Docker tag normalization used by metadata-action.
      tag=$(sed -E 's/[^A-Za-z0-9_.-]+/-/g' <<< "${GITHUB_REF#refs/tags/}")
      tag=${tag:0:128}
      regctl image copy "$primary@$digest" "$primary:$tag"
      if [[ -n "$mirror" ]]; then regctl image copy "$mirror@$digest" "$mirror:$tag"; fi
    fi
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
