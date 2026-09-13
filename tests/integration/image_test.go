//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"
)

// The minimal image must retain the shell entrypoints, runtime data, and the
// GPG agent needed for encryption with its default HOME. The database round
// trips separately exercise the image as a non-root user with HOME=/tmp.
func TestImageRuntime(t *testing.T) {
	if os.Getenv("MONGODB_NATIVE_INTEGRATION") == "1" {
		t.Skip("requires the container image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	docker(t, ctx, "run", "--rm", envOr("BACKUP_TEST_IMAGE", "mongodb-backup-s3:integration"),
		"sh", "-ec", `
mongodump --version
mongorestore --version
test -x /run.sh
test -x /backup.sh
test -x /restore.sh
test -s /etc/ssl/certs/ca-certificates.crt
test -s /usr/share/zoneinfo/Asia/Tokyo
printf 'runtime encryption check' | gpg --batch --pinentry-mode loopback \
    --passphrase test-only --symmetric --output /tmp/check.gpg
result=$(gpg --batch --pinentry-mode loopback --passphrase test-only --decrypt /tmp/check.gpg)
test "$result" = 'runtime encryption check'
`)
}
