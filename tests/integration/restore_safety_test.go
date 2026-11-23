//go:build integration

package integration_test

import (
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRestoreWriteConcernAndEncodedKeys(t *testing.T) {
	ctx, cancel := fixtureContext(t, 3*time.Minute)
	defer cancel()
	f := newFixture(t, ctx)
	source := f.connect(t, f.sourceURI)
	for _, collection := range []string{"probe", "second", "third", "fourth"} {
		if _, err := source.Database("app").Collection(collection).InsertOne(ctx, bson.M{"_id": 1, "value": "original"}); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		"S3_PREFIX": "encoded\x01日本語+%2F", "BACKUP_MODE": "per-database", "MONGODB_DATABASE": "app",
		"BACKUP_KEEP_DAYS": "1", "MONGODB_PARALLEL_COLLECTIONS": "4",
		// Write concern must not restrict the read-only backup operation.
		"MONGODB_URI": f.baseEnv["MONGODB_URI"] + "&w=0",
	}
	// Retention lists the uploaded object, exercising encoded keys on backup too.
	f.run(t, env, "backup")
	target := f.connect(t, f.targetURI)
	f.command(t, target.Database("app"), bson.D{
		{Key: "create", Value: "probe"},
		{Key: "validator", Value: bson.M{"mustExist": bson.M{"$exists": true}}},
	})
	for _, command := range []string{"restore", "restore-auth"} {
		t.Run(command+"_rejects_unacknowledged_writes", func(t *testing.T) {
			env["MONGODB_URI"] = f.targetURI + "&w=0"
			output, err := f.runCommand(t, env, command)
			if err == nil || !strings.Contains(output, "restore requires acknowledged writes") || strings.Contains(output, "Downloading backup") {
				t.Fatalf("unacknowledged restore was not rejected before download: %v\n%s", err, output)
			}
		})
	}
	t.Run("acknowledged_writes_report_server_rejection", func(t *testing.T) {
		env["MONGODB_URI"] = f.targetURI + "&w=1"
		output, err := f.runCommand(t, env, "restore")
		if err == nil || !strings.Contains(output, "Document failed validation") || strings.Contains(output, "Restore complete.") {
			t.Fatalf("server validation failure was not reported: %v\n%s", err, output)
		}
		if n, err := target.Database("app").Collection("probe").CountDocuments(ctx, bson.M{}); err != nil || n != 0 {
			t.Fatalf("rejected collection count=%d, error=%v", n, err)
		}
	})
	t.Run("encoded_latest_restore_and_archive_concurrency", func(t *testing.T) {
		env["MONGODB_URI"] = f.targetURI + "&w=majority"
		env["MONGODB_RESTORE_DROP"] = "true"
		env["MONGODB_PARALLEL_COLLECTIONS"] = "1"
		output, err := f.runCommand(t, env, "restore")
		if err != nil || !strings.Contains(output, "Restore complete.") || !strings.Contains(output, "setting number of parallel collections to 4") {
			t.Fatalf("archive concurrency adjustment/restore failed: %v\n%s", err, output)
		}
		for _, collection := range []string{"probe", "second", "third", "fourth"} {
			var doc struct{ Value string }
			if err := target.Database("app").Collection(collection).FindOne(ctx, bson.M{"_id": 1}).Decode(&doc); err != nil || doc.Value != "original" {
				t.Fatalf("restored %s: value=%q, error=%v", collection, doc.Value, err)
			}
		}
	})
}
