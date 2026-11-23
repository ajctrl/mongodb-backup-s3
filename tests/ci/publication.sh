#!/usr/bin/env bash
# Run against a disposable registry on the loopback interface.
set -euo pipefail
registry=${1:?provide a loopback registry host:port}
[[ "$registry" == 127.0.0.1:* ]]
export RUNNER_TEMP
RUNNER_TEMP=$(mktemp -d)
trap 'rm -rf "$RUNNER_TEMP"' EXIT
export REGCTL_CONFIG=$RUNNER_TEMP/regctl.json
regctl registry set "$registry" --tls disabled
export GITHUB_REPOSITORY=fixture/backup GITHUB_OUTPUT=$RUNNER_TEMP/output GITHUB_REF=refs/heads/main
unset MIRROR_USER
export GITHUB_SHA HEAD_SHA IMAGE_DIGEST
repo=$registry/fixture/backup-$BASHPID
publish=(bash .github/scripts/publish-image.sh)
mkdir -p "$RUNNER_TEMP"/{bin,tested,context}
printf '#!/bin/sh\necho "$HEAD_SHA"\n' > "$RUNNER_TEMP/bin/gh"
chmod +x "$RUNNER_TEMP/bin/gh"
export PATH="$RUNNER_TEMP/bin:$PATH"
printf 'FROM scratch\nCOPY payload /payload\n' > "$RUNNER_TEMP/context/Dockerfile"

make_archives() {
  echo "$1" > "$RUNNER_TEMP/context/payload"
  for arch in amd64 arm64; do
    docker buildx build --platform "linux/$arch" --provenance=mode=max \
      --tag "fixture:$arch" --output "type=oci,dest=$RUNNER_TEMP/tested/image-$arch.tar" "$RUNNER_TEMP/context"
  done
}

make_archives first
sha_a=$(printf 'a%.0s' {1..40})
sha_b=$(printf 'b%.0s' {1..40})
GITHUB_SHA=$sha_a
"${publish[@]}" publish "$repo"
digest_a=$(regctl image digest "$repo:sha-$sha_a")

# Read each provenance statement back from the registry and check its subject
# against the runnable manifest in the original archive.
index=$(regctl manifest get "$repo@$digest_a" --format raw-body)
for arch in amd64 arm64; do
  archive="ocidir://$RUNNER_TEMP/original:$arch"
  regctl image import "$archive" "$RUNNER_TEMP/tested/image-$arch.tar"
  original=$(regctl image digest --platform "linux/$arch" "$archive")
  [[ "$(regctl image digest --platform "linux/$arch" "$repo@$digest_a")" == "$original" ]]
  attestation=$(jq -er --arg subject "$original" '.manifests[] |
    select(.annotations["vnd.docker.reference.digest"] == $subject) | .digest' <<< "$index")
  statement=$(regctl manifest get "$repo@$attestation" --format raw-body |
    jq -er '.layers[] | select(.annotations["in-toto.io/predicate-type"] | startswith("https://slsa.dev/provenance/")) | .digest')
  regctl blob get "$repo" "$statement" | jq -e --arg sha "${original#sha256:}" '
    (.predicateType | startswith("https://slsa.dev/provenance/")) and any(.subject[]; .digest.sha256 == $sha)' >/dev/null
done

make_archives second
GITHUB_SHA=$sha_b
HEAD_SHA=$sha_b
"${publish[@]}" publish "$repo"
IMAGE_DIGEST=$(regctl image digest "$repo:sha-$sha_b")
[[ "$IMAGE_DIGEST" != "$digest_a" ]]
"${publish[@]}" latest "$repo"
# An older run finishes after B's promotion.
GITHUB_SHA=$sha_a IMAGE_DIGEST=$digest_a "${publish[@]}" latest "$repo"
[[ "$(regctl image digest "$repo:latest")" == "$IMAGE_DIGEST" ]]

# A second build for B would produce a different manifest. Reuse B's first
# published index, even if the previous attempt stopped before updating latest.
make_archives retry
"${publish[@]}" publish "$repo"
"${publish[@]}" latest "$repo"
[[ "$(regctl image digest "$repo:sha-$sha_b")" == "$IMAGE_DIGEST" ]]
[[ "$(regctl image digest "$repo:latest")" == "$IMAGE_DIGEST" ]]
