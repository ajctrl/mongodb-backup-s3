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
cat > "$RUNNER_TEMP/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -eu
case "$1" in
  api) echo "$HEAD_SHA" ;;
  attestation)
    [[ "$2" == verify && "$4" == --repo && "$5" == "$GITHUB_REPOSITORY" &&
       "$6" == --signer-workflow && "$7" == "$GITHUB_REPOSITORY/.github/workflows/verify.yml" &&
       "$8" == --source-digest && "$9" == "$GITHUB_SHA" && "${10}" == --deny-self-hosted-runners ]]
    grep -Fxq "$GITHUB_SHA $3" "$RUNNER_TEMP/signed"
    ;;
  *) exit 1 ;;
esac
MOCK
chmod +x "$RUNNER_TEMP/bin/gh"
export PATH="$RUNNER_TEMP/bin:$PATH"
printf 'FROM scratch\nCOPY payload /payload\n' > "$RUNNER_TEMP/context/Dockerfile"

make_archives() {
  echo "$1" > "$RUNNER_TEMP/context/payload"
  for arch in amd64 arm64; do
    docker buildx build --platform "linux/$arch" --provenance=mode=max \
      --label "org.opencontainers.image.revision=$GITHUB_SHA" \
      --label "org.opencontainers.image.source=${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY" \
      --tag "fixture:$arch" --output "type=oci,dest=$RUNNER_TEMP/tested/image-$arch.tar" "$RUNNER_TEMP/context"
  done
}

publish_tested_archives() {
  "${publish[@]}" prepare "$repo"
  local digest
  digest=$(regctl image digest "ocidir://$RUNNER_TEMP/tested-layout:tested")
  echo "$GITHUB_SHA oci://$repo@$digest" >> "$RUNNER_TEMP/signed"
  PREPARED_DIGEST=$digest "${publish[@]}" publish "$repo"
}

sha_a=$(printf 'a%.0s' {1..40})
sha_b=$(printf 'b%.0s' {1..40})
GITHUB_SHA=$sha_a
make_archives first
publish_tested_archives
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

GITHUB_SHA=$sha_b
make_archives second
HEAD_SHA=$sha_b
publish_tested_archives
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

# A release reference accepts retries for its original digest only.
GITHUB_REF=refs/tags/v1.0.0-build
"${publish[@]}" publish "$repo"
"${publish[@]}" publish "$repo"
GITHUB_REF=refs/tags/v1.0.0+build
if "${publish[@]}" publish "$repo"; then exit 1; fi
GITHUB_REF=refs/tags/v1.0.0-build
GITHUB_SHA=$sha_a
if "${publish[@]}" publish "$repo"; then exit 1; fi
[[ "$(regctl image digest "$repo:v1.0.0-build")" == "$IMAGE_DIGEST" ]]

mirror=$registry/fixture/mirror-$BASHPID
GITHUB_REF=refs/heads/main
GITHUB_SHA=$sha_a
"${publish[@]}" publish "$repo" "$mirror"
"${publish[@]}" publish "$repo" "$mirror"
[[ "$(regctl image digest "$mirror:sha-$sha_a")" == "$digest_a" ]]
regctl image copy "$repo@$digest_a" "$mirror:sha-$sha_b"
GITHUB_SHA=$sha_b
if "${publish[@]}" publish "$repo" "$mirror"; then exit 1; fi
[[ "$(regctl image digest "$mirror:sha-$sha_b")" == "$digest_a" ]]

# Neither a single-platform manifest nor a complete index for another commit
# may acquire a release tag merely because it occupies a SHA tag.
GITHUB_SHA=$(printf 'c%.0s' {1..40})
GITHUB_REF=refs/tags/v2.0.0
single=$(regctl image digest --platform linux/amd64 "$repo@$digest_a")
for invalid in "$single" "$digest_a"; do
  echo "$GITHUB_SHA oci://$repo@$invalid" >> "$RUNNER_TEMP/signed"
  regctl image copy "$repo@$invalid" "$repo:sha-$GITHUB_SHA"
  if "${publish[@]}" publish "$repo"; then exit 1; fi
  if regctl image digest "$repo:v2.0.0" 2>/dev/null; then exit 1; fi
done

# A complete, correctly labeled index for this commit still needs a trusted signature.
make_archives unsigned
"${publish[@]}" prepare "$repo"
regctl image copy "ocidir://$RUNNER_TEMP/tested-layout:tested" "$repo:sha-$GITHUB_SHA"
if "${publish[@]}" publish "$repo"; then exit 1; fi
if regctl image digest "$repo:v2.0.0" 2>/dev/null; then exit 1; fi
