#!/usr/bin/env bash
set -euo pipefail

# Test the actual README service definition with role-based and static credentials.
scratch=$(mktemp -d)
project=mongodb-backup-test-${scratch##*.}
compose=(docker compose -p "${project,,}" --env-file template.env -f docker-compose.yaml)
trap '"${compose[@]}" down --volumes >/dev/null; rm -rf "$scratch"' EXIT
awk '/^### Docker Compose$/{section=1} section && /^```yaml$/{copy=1; next} copy && /^```$/{exit} copy' README.md > "$scratch/example.yaml"
for value in '' fixture; do
  MONGODB_PASSWORD=fixture S3_ACCESS_KEY_ID=$value S3_SECRET_ACCESS_KEY=$value \
    docker compose --env-file /dev/null -p example -f "$scratch/example.yaml" config --format json |
    jq -e --arg expected "$value" '.services.backup.environment | .S3_ACCESS_KEY_ID == $expected and .S3_SECRET_ACCESS_KEY == $expected' >/dev/null
done

init=("${compose[@]}" run --rm --no-deps -T config-init)
"${init[@]}"
original=$("${init[@]}" sha256sum /bootstrap/keyFile)
"${init[@]}"
[[ $("${init[@]}" sha256sum /bootstrap/keyFile) == "$original" ]]
"${init[@]}" sh -ec 'chmod 600 /bootstrap/keyFile && : > /bootstrap/keyFile'
if "${init[@]}"; then
  echo 'An empty replica set key was accepted' >&2
  exit 1
fi
