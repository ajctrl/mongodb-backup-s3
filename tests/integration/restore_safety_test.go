//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
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

func TestFixedBackupPreservesMutableNullVersion(t *testing.T) {
	ctx, cancel := fixtureContext(t, 2*time.Minute)
	defer cancel()
	f := newFixture(t, ctx)
	env := map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASE": "app", "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": "null-version"}
	const key = "null-version/per-database/app/latest.zip"
	setVersioning := func(status types.BucketVersioningStatus) {
		t.Helper()
		_, err := f.s3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(f.bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: status}})
		if err != nil {
			t.Fatal(err)
		}
	}
	nullContents := func() []byte {
		t.Helper()
		obj, err := f.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key), VersionId: aws.String("null")})
		if err != nil {
			t.Fatal(err)
		}
		defer obj.Body.Close()
		b, err := io.ReadAll(obj.Body)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	src := f.connect(t, f.sourceURI)
	f.seed(t, src)
	setVersioning(types.BucketVersioningStatusSuspended)
	env["BACKUP_ALLOW_UNVERSIONED_OVERWRITE"] = "true"
	f.run(t, env, "backup")
	original := nullContents()
	setVersioning(types.BucketVersioningStatusEnabled)
	// Bury the null version under an immutable current version.
	f.run(t, env, "backup")
	immutable := f.version(t, key)
	delete(env, "BACKUP_ALLOW_UNVERSIONED_OVERWRITE")
	f.setValue(t, src, "app", "updated")
	cmd, _ := f.backupCommand(t, env, "backup")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(pipe)
	suspended := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "Creating and uploading backup") {
			setVersioning(types.BucketVersioningStatusSuspended)
			suspended = true
		}
	}
	if err := cmd.Wait(); err != nil || scanner.Err() != nil || !suspended {
		t.Fatalf("backup transition: %v, %v, suspended=%t", err, scanner.Err(), suspended)
	}
	if !bytes.Equal(original, nullContents()) || f.version(t, key) != immutable || len(f.keys(t, "null-version/")) != 2 {
		t.Fatal("expected both original fixed versions and a timestamp generation")
	}
	env["MONGODB_URI"] = f.targetURI
	env["MONGODB_RESTORE_DROP"] = "true"
	f.run(t, env, "restore")
	f.assertValue(t, f.connect(t, f.targetURI), "app", "updated")
	f.run(t, env, "restore", "--version-id", immutable)
	f.assertValue(t, f.connect(t, f.targetURI), "app", "original")
	if out, err := f.runCommand(t, env, "restore", "--version-id", "null"); err == nil || !strings.Contains(out, "mutable") || strings.Contains(out, "Downloading") {
		t.Fatalf("null version preflight: %v %s", err, out)
	}
}

func TestConcurrentFixedBackupsSurviveSuspension(t *testing.T) {
	ctx, cancel := fixtureContext(t, 3*time.Minute)
	defer cancel()
	f := newFixture(t, ctx)
	src := f.connect(t, f.sourceURI)
	f.seed(t, src)
	env := map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASE": "app", "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": "concurrent"}
	f.command(t, src.Database("admin"), bson.D{
		{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: bson.M{"times": 2}},
		{Key: "data", Value: bson.M{"failCommands": bson.A{"find"}, "blockConnection": true, "blockTimeMS": 8000, "appName": "mongodump"}},
	})
	started := make(chan struct{}, 2)
	finished := make(chan error, 2)
	for range 2 {
		cmd, _ := f.backupCommand(t, env, "backup")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				if strings.Contains(scanner.Text(), "Creating and uploading backup") {
					started <- struct{}{}
				}
			}
			finished <- cmd.Wait()
		}()
	}
	for range 2 {
		select {
		case <-started:
		case err := <-finished:
			t.Fatalf("backup ended before both uploads started: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	_, err := f.s3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(f.bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	keys := f.keys(t, "concurrent/")
	if len(keys) != 2 {
		t.Fatalf("expected two independent generations: %q", keys)
	}
	env["MONGODB_URI"], env["MONGODB_RESTORE_DROP"], env["BACKUP_FILENAME_MODE"] = f.targetURI, "true", "timestamp"
	target := f.connect(t, f.targetURI)
	for _, key := range keys {
		f.setValue(t, target, "app", "before restore")
		generation := strings.TrimSuffix(strings.TrimPrefix(key, "concurrent/per-database/app/"), ".zip")
		f.run(t, env, "restore", generation)
		f.assertValue(t, target, "app", "original")
	}
}
