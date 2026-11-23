//go:build integration

package integration_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPartialBackupRecovery(t *testing.T) {
	ctx, cancel := fixtureContext(t, 4*time.Minute)
	defer cancel()
	f := newFixture(t, ctx)
	source := f.connect(t, f.sourceURI)
	f.seed(t, source)
	f.command(t, source.Database("admin"), bson.D{
		{Key: "createUser", Value: "partialreader"}, {Key: "pwd", Value: testPassword},
		{Key: "roles", Value: bson.A{bson.M{"db": "app", "role": "read"}}},
	})

	const prefix = "partial-recovery/"
	env := map[string]string{"S3_PREFIX": prefix, "BACKUP_FILENAME_MODE": "fixed", "BACKUP_ALLOW_UNVERSIONED_OVERWRITE": "true", "BACKUP_KEEP_DAYS": "1"}
	f.run(t, env, "backup")
	fullVersion := f.version(t, prefix+"full/latest.zip")

	env["MONGODB_URI"] = strings.Replace(f.sourceURI, "root:", "partialreader:", 1)
	log, err := f.runCommand(t, env, "backup")
	if err == nil || !strings.Contains(log, "getCmdLineOpts") || len(f.keys(t, prefix+"partial/")) != 0 {
		t.Fatalf("unverified server role: %v %s", err, log)
	}
	admin := source.Database("admin")
	f.command(t, admin, bson.D{{Key: "createRole", Value: "verifyBackupRole"}, {Key: "privileges", Value: bson.A{bson.M{"resource": bson.M{"cluster": true}, "actions": bson.A{"getCmdLineOpts"}}}}, {Key: "roles", Value: bson.A{}}})
	f.command(t, admin, bson.D{{Key: "grantRolesToUser", Value: "partialreader"}, {Key: "roles", Value: bson.A{"verifyBackupRole"}}})
	backupPartial := func() {
		t.Helper()
		log, err := f.runCommand(t, env, "backup")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(log, `saved partial databases: ["app"]`) {
			t.Fatalf("partial backup result: %v\n%s", err, log)
		}
	}
	backupPartial()
	const partialKey = prefix + "partial/app/latest.zip"
	firstVersion := f.version(t, partialKey)

	// Read versions independently of the unavailable replica set configuration.
	privileges := bson.A{bson.M{"resource": bson.M{"cluster": true}, "actions": bson.A{"getParameter"}}}
	for _, collection := range []string{"system.users", "system.roles", "system.version", "system.keys"} {
		privileges = append(privileges, bson.M{"resource": bson.M{"db": "admin", "collection": collection}, "actions": bson.A{"find", "listIndexes", "collStats"}})
	}
	f.command(t, admin, bson.D{{Key: "createRole", Value: "partialMetadata"}, {Key: "privileges", Value: privileges}, {Key: "roles", Value: bson.A{}}})
	roles := bson.A{"partialMetadata"}
	for _, db := range []string{"admin", "config", "local"} {
		roles = append(roles, bson.M{"db": db, "role": "read"})
	}
	f.command(t, admin, bson.D{{Key: "grantRolesToUser", Value: "partialreader"}, {Key: "roles", Value: roles}})
	f.setValue(t, source, "app", "second")
	backupPartial()
	if f.version(t, partialKey) == firstVersion {
		t.Fatal("fixed partial key did not get a second S3 version")
	}
	env["BACKUP_FILENAME_MODE"] = "timestamp"
	f.setValue(t, source, "app", "timestamp")
	backupPartial()
	keys := f.keys(t, prefix+"partial/")
	if len(keys) != 2 || keys[1] != partialKey || !strings.HasPrefix(keys[0], prefix+"partial/app/") || f.version(t, prefix+"full/latest.zip") != fullVersion {
		t.Fatalf("unexpected generations after partial backups: %v", keys)
	}
	generation := strings.TrimSuffix(strings.TrimPrefix(keys[0], prefix+"partial/app/"), ".zip")
	env["BACKUP_MODE"], env["MONGODB_DATABASE"] = "partial", "app"
	env["MONGODB_URI"], env["MONGODB_RESTORE_DROP"] = f.targetURI, "true"
	target := f.connect(t, f.targetURI)
	f.run(t, env, "restore", generation)
	f.assertValue(t, target, "app", "timestamp")
	env["BACKUP_FILENAME_MODE"] = "fixed"
	f.run(t, env, "restore", "--version-id", firstVersion)
	f.assertValue(t, target, "app", "original")
	f.run(t, env, "restore")
	f.assertValue(t, target, "app", "timestamp")
	if n, err := target.Database("analytics").Collection("probe").CountDocuments(ctx, bson.M{}); err != nil || n != 0 {
		t.Fatalf("partial restore touched analytics: count=%d err=%v", n, err)
	}
}
