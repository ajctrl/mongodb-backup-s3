#!/usr/bin/env bash
set -euo pipefail
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GITHUB_REPOSITORY=fixture/backup RENOVATE_BOT_LOGIN='deps-bot[bot]' CALLS=$work/calls
cat > "$work/gh" <<'MOCK'
#!/usr/bin/env bash
set -eu
case "$*" in
  'api --paginate repos/fixture/backup/pulls?state=open&base=main&per_page=100')
    echo '[
      {"number":1,"user":{"login":"deps-bot[bot]"},"head":{"ref":"renovate/conflict","sha":"aaa","repo":{"full_name":"fixture/backup"}}},
      {"number":2,"user":{"login":"deps-bot[bot]"},"head":{"ref":"renovate/behind","sha":"bbb","repo":{"full_name":"fixture/backup"}}},
      {"number":3,"user":{"login":"deps-bot[bot]"},"head":{"ref":"renovate/current","sha":"ccc","repo":{"full_name":"fixture/backup"}}},
      {"number":4,"user":{"login":"deps-bot"},"head":{"ref":"renovate/other","sha":"ddd","repo":{"full_name":"fixture/backup"}}},
      {"number":5,"user":{"login":"deps-bot[bot]"},"head":{"ref":"renovate/fork","sha":"eee","repo":{"full_name":"fork/backup"}}}
    ]' ;;
  *compare/main...aaa*|*compare/main...bbb*) echo 1 ;;
  *compare/main...ccc*) echo 0 ;;
  *pulls/1/update-branch*expected_head_sha=aaa) echo 1 >> "$CALLS"; echo 'HTTP 409' >&2; exit 1 ;;
  *pulls/2/update-branch*expected_head_sha=bbb) echo 2 >> "$CALLS" ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
MOCK
chmod +x "$work/gh"
PATH="$work:$PATH" bash .github/scripts/refresh-renovate-prs.sh
diff -u <(printf '1\n2\n') "$CALLS"
