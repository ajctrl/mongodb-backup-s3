//go:build integration

package integration_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

const (
	defaultS3TestImage = "quay.io/minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e"
	testPassword       = "integration-password"
	testReplicaKey     = "aW50ZWdyYXRpb24tb25seS1yZXBsaWNhLWtleQ=="
	testConfig         = "replication:\n  replSetName: source\nsecurity:\n  authorization: enabled\n"
)

// TestBackupRestore creates disposable authenticated source and destination
// replica sets and a versioned MinIO bucket. By default it requires Docker and
// an already-built mongodb-backup-s3:integration image. Set
// MONGODB_NATIVE_INTEGRATION=1 to use local mongod, minio and backup binaries;
// override paths with MONGOD_TEST_BINARY, MINIO_TEST_BINARY, BACKUP_TEST_BINARY.
// No existing database, container or bucket is modified by this suite.
func TestBackupRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	f := newFixture(t, ctx)
	source := f.connect(t, f.sourceURI)
	f.seed(t, source)

	if !t.Run("full_oplog_concurrent_writes_and_auth", func(t *testing.T) {
		var completed atomic.Int64
		writerCtx, stopWriter := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			session, err := source.StartSession()
			if err != nil {
				done <- err
				return
			}
			defer session.EndSession(context.Background())
			for writerCtx.Err() == nil {
				seq := completed.Load() + 1
				_, err := session.WithTransaction(writerCtx, func(txCtx context.Context) (any, error) {
					for _, database := range []string{"app", "analytics"} {
						_, err := source.Database(database).Collection("clock").UpdateOne(txCtx,
							bson.M{"_id": "clock"}, bson.M{"$set": bson.M{"sequence": seq}})
						if err != nil {
							return nil, err
						}
					}
					return nil, nil
				})
				if err != nil {
					if writerCtx.Err() != nil {
						done <- nil
					} else {
						done <- err
					}
					return
				}
				completed.Store(seq)
			}
			done <- nil
		}()
		defer stopWriter()
		waitFor(t, ctx, "concurrent writer", func(context.Context) error {
			if completed.Load() == 0 {
				return fmt.Errorf("no committed transaction yet")
			}
			return nil
		})
		before := completed.Load()
		env := map[string]string{"S3_PREFIX": "full-concurrent"}
		f.run(t, env, "backup")
		after := completed.Load()
		stopWriter()
		if err := <-done; err != nil {
			t.Fatalf("concurrent transaction writer: %v", err)
		}
		if after <= before {
			t.Fatal("no transactions committed while the backup command was running")
		}
		keys := f.keys(t, "full-concurrent/")
		if len(keys) != 1 || !strings.HasPrefix(keys[0], "full-concurrent/full/") || !strings.HasSuffix(keys[0], ".zip") {
			t.Fatalf("unexpected full backup objects: %v", keys)
		}
		f.assertBundle(t, keys[0], false)
		env["MONGODB_URI"] = f.targetURI
		env["MONGODB_RESTORE_DROP"] = "true"
		timestamp := strings.TrimSuffix(filepath.Base(keys[0]), ".zip")
		f.run(t, env, "restore", timestamp)
		// The restore replaces the destination root user's identity as well as
		// its credentials. Verify through a newly authenticated connection.
		target := f.connect(t, f.targetURI)
		var restoredSequence int64
		for _, database := range []string{"app", "analytics"} {
			var clock struct{ Sequence int64 }
			if err := target.Database(database).Collection("clock").FindOne(ctx, bson.M{"_id": "clock"}).Decode(&clock); err != nil {
				t.Fatal(err)
			}
			if database == "app" {
				restoredSequence = clock.Sequence
			} else if clock.Sequence != restoredSequence {
				t.Fatalf("oplog replay did not preserve a transaction across databases: app=%d analytics=%d", restoredSequence, clock.Sequence)
			}
		}
		if restoredSequence < before || restoredSequence > after+1 {
			t.Fatalf("restored transaction sequence %d outside backup interval [%d, %d]", restoredSequence, before, after+1)
		}
		f.assertValue(t, target, "app", "original")
		f.assertValue(t, target, "analytics", "analytics original")
		f.assertIndex(t, target)
		f.assertAuth(t, "app", "appreader")
		f.assertAuth(t, "admin", "adminreader")
		if got, err := target.Database("app").Collection("payload").CountDocuments(ctx, bson.M{}); err != nil || got != 1024 {
			t.Fatalf("restored payload count=%d error=%v", got, err)
		}
	}) {
		return
	}

	target := f.connect(t, f.targetURI)
	if !t.Run("per_database_data_and_explicit_auth_restore", func(t *testing.T) {
		env := map[string]string{
			"S3_PREFIX": "selected", "BACKUP_MODE": "per-database", "BACKUP_FILENAME_MODE": "fixed",
			"MONGODB_DATABASES": "app,analytics",
		}
		f.run(t, env, "backup")
		keys := f.keys(t, "selected/")
		want := "selected/per-database/analytics/latest.zip,selected/per-database/app/latest.zip"
		if strings.Join(keys, ",") != want {
			t.Fatalf("DB selection produced %v; want %s", keys, want)
		}
		for _, key := range keys {
			f.assertBundle(t, key, true)
		}
		f.setValue(t, target, "app", "changed")
		f.setValue(t, target, "analytics", "leave this alone")
		for authDB, user := range map[string]string{"app": "appreader", "admin": "adminreader"} {
			f.command(t, target.Database(authDB), bson.D{{Key: "dropUser", Value: user}})
		}
		env["MONGODB_URI"] = f.targetURI
		env["MONGODB_DATABASES"] = ""
		env["MONGODB_DATABASE"] = "app"
		env["MONGODB_RESTORE_DROP"] = "true"
		f.run(t, env, "restore")
		f.assertValue(t, target, "app", "original")
		f.assertValue(t, target, "analytics", "leave this alone")
		var users struct {
			Users []bson.M `bson:"users"`
		}
		if err := target.Database("app").RunCommand(ctx, bson.D{{Key: "usersInfo", Value: "appreader"}}).Decode(&users); err != nil {
			t.Fatal(err)
		}
		if len(users.Users) != 0 {
			t.Fatal("ordinary per-database restore unexpectedly changed authentication")
		}
		f.run(t, env, "restore-auth")
		f.assertAuth(t, "app", "appreader")
		f.assertAuth(t, "admin", "adminreader")
		f.assertValue(t, target, "analytics", "leave this alone")
	}) {
		return
	}

	t.Run("database_discovery_and_exclusion", func(t *testing.T) {
		f.run(t, map[string]string{
			"S3_PREFIX": "discovered", "BACKUP_MODE": "per-database", "BACKUP_FILENAME_MODE": "fixed",
			"MONGODB_BACKUP_ALL": "true", "MONGODB_DATABASES_EXCLUDE": "excluded",
		}, "backup")
		want := "discovered/per-database/analytics/latest.zip,discovered/per-database/app/latest.zip"
		if got := strings.Join(f.keys(t, "discovered/"), ","); got != want {
			t.Fatalf("discovered backups=%s, want %s", got, want)
		}
	})

	t.Run("encrypted_fixed_versions_and_config_extract", func(t *testing.T) {
		if f.native {
			if _, err := exec.LookPath("gpg"); err != nil {
				t.Skip("native encrypted integration requires gpg on PATH")
			}
		}
		env := map[string]string{
			"S3_PREFIX": "encrypted", "BACKUP_MODE": "per-database", "MONGODB_DATABASE": "app",
			"BACKUP_FILENAME_MODE": "fixed", "PASSPHRASE": "integration encryption passphrase",
		}
		const key = "encrypted/per-database/app/latest.zip.gpg"
		f.setValue(t, source, "app", "first version")
		f.run(t, env, "backup")
		firstVersion := f.version(t, key)
		f.setValue(t, source, "app", "second version")
		f.run(t, env, "backup")
		if f.version(t, key) == firstVersion {
			t.Fatal("a second fixed backup did not produce a new S3 version")
		}
		env["MONGODB_URI"] = f.targetURI
		env["MONGODB_RESTORE_DROP"] = "true"
		f.run(t, env, "restore", "--version-id", firstVersion)
		f.assertValue(t, target, "app", "first version")
		f.run(t, env, "restore")
		f.assertValue(t, target, "app", "second version")
		f.run(t, env, "extract-config", f.outputPath, "--version-id", firstVersion)
		// Configuration extraction must preserve the archived content without
		// applying the source replica-set configuration to the destination.
		found := false
		err := filepath.WalkDir(f.outputDir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Name() == "mongod.conf" && !entry.IsDir() {
				content, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if string(content) != testConfig {
					return fmt.Errorf("extracted config differs: %q", content)
				}
				found = true
			}
			return nil
		})
		if err != nil || !found {
			t.Fatalf("extract-config did not recover mongod.conf: found=%t error=%v", found, err)
		}
		var hello struct {
			SetName string `bson:"setName"`
		}
		if err := target.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil || hello.SetName != "destination" {
			t.Fatalf("configuration extraction altered destination replica set: %+v error=%v", hello, err)
		}
	})

	t.Run("failed_config_does_not_replace_successful_backup", func(t *testing.T) {
		env := map[string]string{
			"S3_PREFIX": "failed-config", "BACKUP_MODE": "per-database", "MONGODB_DATABASE": "app",
			"BACKUP_FILENAME_MODE": "fixed",
		}
		f.run(t, env, "backup")
		const key = "failed-config/per-database/app/latest.zip"
		before := f.version(t, key)
		env["MONGODB_CONFIG_DIR"] = "/nonexistent-integration-config"
		output, err := f.runCommand(t, env, "backup")
		if err == nil {
			t.Fatalf("backup with missing configured files unexpectedly succeeded: %s", output)
		}
		if after := f.version(t, key); before != after {
			t.Fatal("failed backup replaced the previously successful object")
		}
	})
}

type fixture struct {
	ctx                              context.Context
	native                           bool
	name, image, anchor, binary      string
	sourceURI, targetURI, endpoint   string
	configDir, outputDir, outputPath string
	bucket                           string
	s3                               *s3.Client
	baseEnv                          map[string]string
	runNumber                        int
}

func newFixture(t *testing.T, ctx context.Context) *fixture {
	t.Helper()
	f := &fixture{ctx: ctx, native: os.Getenv("MONGODB_NATIVE_INTEGRATION") == "1", name: fmt.Sprintf("mongos3-integration-%d", time.Now().UnixNano())}
	f.configDir, f.outputDir = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(f.configDir, "mongod.conf"), []byte(testConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.configDir, "keyFile"), []byte(testReplicaKey), 0600); err != nil {
		t.Fatal(err)
	}
	sourcePort, targetPort, s3Port := unusedPort(t), unusedPort(t), unusedPort(t)
	f.endpoint = "http://127.0.0.1:" + s3Port
	f.sourceURI = authenticatedURI(sourcePort, "source")
	f.targetURI = authenticatedURI(targetPort, "destination")
	if f.native {
		f.binary = requireBinary(t, "BACKUP_TEST_BINARY", "mongodb-backup-s3")
		mongod := requireBinary(t, "MONGOD_TEST_BINARY", "mongod")
		minio := requireBinary(t, "MINIO_TEST_BINARY", "minio")
		for _, server := range []struct{ port, set string }{{sourcePort, "source"}, {targetPort, "destination"}} {
			dir := t.TempDir()
			key := filepath.Join(dir, "keyFile")
			if err := os.WriteFile(key, []byte(testReplicaKey), 0600); err != nil {
				t.Fatal(err)
			}
			// Enforce MongoDB's key-file requirement on filesystems with umasks.
			if err := os.Chmod(key, 0600); err != nil {
				t.Fatal(err)
			}
			startProcess(t, mongod, nil, "--dbpath", dir, "--bind_ip", "127.0.0.1", "--port", server.port,
				"--replSet", server.set, "--keyFile", key, "--oplogSize", "128")
		}
		startProcess(t, minio, []string{"MINIO_ROOT_USER=integration", "MINIO_ROOT_PASSWORD=" + testPassword},
			"server", "--address", "127.0.0.1:"+s3Port, "--console-address", "127.0.0.1:"+unusedPort(t), t.TempDir())
		f.outputPath = filepath.Join(f.outputDir, "extracted")
	} else {
		requireBinary(t, "", "docker")
		docker(t, ctx, "info", "--format", "{{.ServerVersion}}")
		f.image = envOr("BACKUP_TEST_IMAGE", "mongodb-backup-s3:integration")
		docker(t, ctx, "image", "inspect", f.image)
		f.anchor = f.name + "-source"
		for _, name := range []string{f.anchor, f.name + "-destination", f.name + "-s3"} {
			t.Cleanup(func() { dockerCleanup(t, "rm", "-f", "-v", name) })
		}
		mongoImage := envOr("MONGODB_TEST_IMAGE", "mongo:8.0")
		startMongo := func(name, port, set string, networkArgs []string) {
			args := append([]string{"run", "-d", "--name", name}, networkArgs...)
			args = append(args, "-e", "MONGO_INITDB_ROOT_USERNAME=root", "-e", "MONGO_INITDB_ROOT_PASSWORD="+testPassword)
			args = append(args, "--entrypoint", "bash", mongoImage, "-c",
				"printf '%s' "+testReplicaKey+" >/tmp/test-key; chmod 600 /tmp/test-key; chown mongodb:mongodb /tmp/test-key; exec docker-entrypoint.sh mongod --bind_ip_all --port "+port+" --replSet "+set+" --keyFile /tmp/test-key --oplogSize 128")
			docker(t, ctx, args...)
		}
		startMongo(f.anchor, sourcePort, "source", []string{
			"-p", "127.0.0.1:" + sourcePort + ":" + sourcePort,
			"-p", "127.0.0.1:" + targetPort + ":" + targetPort,
			"-p", "127.0.0.1:" + s3Port + ":" + s3Port,
		})
		// The official entrypoint temporarily uses port 27017 for initialization.
		// Finish the source first because the destination shares its namespace.
		f.initializeReplica(t, sourcePort, "source")
		startMongo(f.name+"-destination", targetPort, "destination", []string{"--network", "container:" + f.anchor})
		docker(t, ctx, "run", "-d", "--name", f.name+"-s3", "--network", "container:"+f.anchor,
			"-e", "MINIO_ROOT_USER=integration", "-e", "MINIO_ROOT_PASSWORD="+testPassword,
			envOr("S3_TEST_IMAGE", defaultS3TestImage), "server", "--address", ":"+s3Port, "/data")
		f.outputPath = "/output/extracted"
	}
	for _, server := range []struct{ port, set string }{{sourcePort, "source"}, {targetPort, "destination"}} {
		if !f.native && server.set == "source" {
			continue
		}
		f.initializeReplica(t, server.port, server.set)
	}
	f.s3 = s3.NewFromConfig(aws.Config{
		Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("integration", testPassword, ""),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(f.endpoint)
		o.UsePathStyle = true
		o.RetryMaxAttempts = 1
	})
	waitFor(t, ctx, "MinIO", func(ctx context.Context) error {
		_, err := f.s3.ListBuckets(ctx, &s3.ListBucketsInput{})
		return err
	})
	f.bucket = f.name
	if _, err := f.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(f.bucket)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(f.bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}
	configPath := f.configDir
	if !f.native {
		configPath = "/fixtures"
	}
	f.baseEnv = map[string]string{
		"MONGODB_URI": f.sourceURI, "MONGODB_CONFIG_DIR": configPath,
		"MONGODB_READ_PREFERENCE": "primary", "MONGODB_TLS_CA_FILE": "", "MONGODB_RESTORE_DROP": "false", "MONGODB_PARALLEL_COLLECTIONS": "4",
		"S3_BUCKET": f.bucket, "S3_REGION": "us-east-1", "S3_ENDPOINT": f.endpoint,
		"S3_ACCESS_KEY_ID": "integration", "S3_SECRET_ACCESS_KEY": testPassword, "S3_SESSION_TOKEN": "",
		"S3_UPLOAD_PART_SIZE_MB": "5", "BACKUP_MODE": "full", "BACKUP_FILENAME_MODE": "timestamp",
		"SCHEDULE": "", "PASSPHRASE": "", "BACKUP_KEEP_DAYS": "", "MONGODB_URI_FILE": "",
		"MONGODB_DATABASE": "", "MONGODB_DATABASES": "", "MONGODB_BACKUP_ALL": "false", "MONGODB_DATABASES_EXCLUDE": "",
	}
	return f
}

func (f *fixture) initializeReplica(t *testing.T, port, set string) {
	t.Helper()
	uri := "mongodb://127.0.0.1:" + port + "/?directConnection=true"
	if !f.native {
		uri = "mongodb://root:" + testPassword + "@127.0.0.1:" + port + "/?authSource=admin&directConnection=true"
	}
	client := f.connect(t, uri)
	waitFor(t, f.ctx, "mongod "+set, func(ctx context.Context) error { return client.Ping(ctx, readpref.PrimaryPreferred()) })
	f.command(t, client.Database("admin"), bson.D{{Key: "replSetInitiate", Value: bson.M{
		"_id": set, "members": bson.A{bson.M{"_id": 0, "host": "127.0.0.1:" + port}},
	}}})
	waitFor(t, f.ctx, "primary "+set, func(ctx context.Context) error {
		var hello struct {
			Primary bool `bson:"isWritablePrimary"`
		}
		if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
			return err
		}
		if !hello.Primary {
			return fmt.Errorf("primary election pending")
		}
		return nil
	})
	if f.native {
		f.command(t, client.Database("admin"), bson.D{
			{Key: "createUser", Value: "root"}, {Key: "pwd", Value: testPassword}, {Key: "roles", Value: bson.A{"root"}},
		})
	}
	// oplog replay needs anyAction on anyResource. Grant it explicitly to this
	// disposable fixture account; the source grant survives authentication restore.
	admin := f.connect(t, authenticatedURI(port, set)).Database("admin")
	f.command(t, admin, bson.D{
		{Key: "createRole", Value: "integrationOplogRestore"},
		{Key: "privileges", Value: bson.A{bson.M{"resource": bson.M{"anyResource": true}, "actions": bson.A{"anyAction"}}}},
		{Key: "roles", Value: bson.A{}},
	})
	f.command(t, admin, bson.D{
		{Key: "grantRolesToUser", Value: "root"}, {Key: "roles", Value: bson.A{"integrationOplogRestore"}},
	})
}

func (f *fixture) connect(t *testing.T, uri string) *mongo.Client {
	t.Helper()
	// Direct mode allows the Go fixture to work through Docker published ports.
	// The backup process itself uses replica-set discovery in its shared network.
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetDirect(true).SetServerSelectionTimeout(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Disconnect(ctx)
	})
	return client
}

func (f *fixture) seed(t *testing.T, client *mongo.Client) {
	t.Helper()
	for database, value := range map[string]string{"app": "original", "analytics": "analytics original", "excluded": "excluded original"} {
		if _, err := client.Database(database).Collection("probe").InsertOne(f.ctx, bson.M{"_id": 1, "value": value, "payload": []byte{0, 1, 255, 39, 92, 10}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, database := range []string{"app", "analytics"} {
		if _, err := client.Database(database).Collection("clock").InsertOne(f.ctx, bson.M{"_id": "clock", "sequence": int64(0)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.Database("app").Collection("probe").Indexes().CreateOne(f.ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "value", Value: 1}}, Options: options.Index().SetName("unique_value").SetUnique(true),
	}); err != nil {
		t.Fatal(err)
	}
	f.command(t, client.Database("app"), bson.D{
		{Key: "createRole", Value: "probeReader"},
		{Key: "privileges", Value: bson.A{bson.M{"resource": bson.M{"db": "app", "collection": "probe"}, "actions": bson.A{"find"}}}},
		{Key: "roles", Value: bson.A{}},
	})
	for authDB, user := range map[string]string{"app": "appreader", "admin": "adminreader"} {
		f.command(t, client.Database(authDB), bson.D{
			{Key: "createUser", Value: user}, {Key: "pwd", Value: testPassword},
			{Key: "roles", Value: bson.A{bson.M{"role": "probeReader", "db": "app"}}},
		})
	}
	// Incompressible payload exercises multipart upload and keeps the dump long
	// enough to observe writes without introducing timing-dependent sleeps.
	docs := make([]any, 1024)
	for i := range docs {
		payload := make([]byte, 8192)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		docs[i] = bson.M{"_id": i, "payload": payload}
	}
	if _, err := client.Database("app").Collection("payload").InsertMany(f.ctx, docs); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) command(t *testing.T, database *mongo.Database, command any) {
	t.Helper()
	if err := database.RunCommand(f.ctx, command).Err(); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) setValue(t *testing.T, client *mongo.Client, database, value string) {
	t.Helper()
	if _, err := client.Database(database).Collection("probe").UpdateOne(f.ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"value": value}}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) assertValue(t *testing.T, client *mongo.Client, database, want string) {
	t.Helper()
	var probe struct {
		Value   string
		Payload []byte
	}
	if err := client.Database(database).Collection("probe").FindOne(f.ctx, bson.M{"_id": 1}).Decode(&probe); err != nil {
		t.Fatal(err)
	}
	if probe.Value != want || !bytes.Equal(probe.Payload, []byte{0, 1, 255, 39, 92, 10}) {
		t.Fatalf("%s restored probe=%+v, want value=%q with original binary payload", database, probe, want)
	}
}

func (f *fixture) assertIndex(t *testing.T, client *mongo.Client) {
	t.Helper()
	_, err := client.Database("app").Collection("probe").InsertOne(f.ctx, bson.M{"_id": 2, "value": "original"})
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("restored unique index did not reject duplicate value: %v", err)
	}
}

func (f *fixture) assertAuth(t *testing.T, authDB, user string) {
	t.Helper()
	uri := strings.Replace(f.targetURI, "root:", user+":", 1)
	uri = strings.Replace(uri, "authSource=admin", "authSource="+authDB, 1)
	client := f.connect(t, uri)
	if err := client.Database("app").Collection("probe").FindOne(f.ctx, bson.M{"_id": 1}).Err(); err != nil {
		t.Fatalf("restored %s user in %s cannot read with original credentials: %v", user, authDB, err)
	}
	_, err := client.Database("app").Collection("probe").InsertOne(f.ctx, bson.M{"_id": "forbidden"})
	var serverErr mongo.ServerError
	if !errors.As(err, &serverErr) || !serverErr.HasErrorCode(13) {
		t.Fatalf("restored read-only role unexpectedly allows write or fails for another reason: %v", err)
	}
}

func (f *fixture) run(t *testing.T, overrides map[string]string, args ...string) string {
	t.Helper()
	output, err := f.runCommand(t, overrides, args...)
	if err != nil {
		t.Fatalf("backup command %v: %v\n%s", args, err, output)
	}
	t.Logf("%v:\n%s", args, strings.TrimSpace(output))
	return output
}

func (f *fixture) runCommand(t *testing.T, overrides map[string]string, args ...string) (string, error) {
	t.Helper()
	env := make(map[string]string, len(f.baseEnv)+len(overrides))
	for key, value := range f.baseEnv {
		env[key] = value
	}
	for key, value := range overrides {
		env[key] = value
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var cmd *exec.Cmd
	if f.native {
		cmd = exec.CommandContext(f.ctx, f.binary, args...)
		cmd.Env = os.Environ()
		for _, key := range keys {
			cmd.Env = append(cmd.Env, key+"="+env[key])
		}
	} else {
		f.runNumber++
		name := fmt.Sprintf("%s-run-%d", f.name, f.runNumber)
		defer dockerCleanup(t, "rm", "-f", "-v", name)
		dockerArgs := []string{"run", "--rm", "--name", name, "--network", "container:" + f.anchor,
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"-e", "HOME=/tmp",
			"--mount", "type=bind,source=" + f.configDir + ",target=/fixtures,readonly",
			"--mount", "type=bind,source=" + f.outputDir + ",target=/output"}
		for _, key := range keys {
			dockerArgs = append(dockerArgs, "-e", key+"="+env[key])
		}
		dockerArgs = append(dockerArgs, f.image, "mongodb-backup-s3")
		dockerArgs = append(dockerArgs, args...)
		cmd = exec.CommandContext(f.ctx, "docker", dockerArgs...)
	}
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (f *fixture) keys(t *testing.T, prefix string) []string {
	t.Helper()
	objects, err := f.s3.ListObjectsV2(f.ctx, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket), Prefix: aws.String(prefix)})
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(objects.Contents))
	for _, object := range objects.Contents {
		keys = append(keys, aws.ToString(object.Key))
	}
	sort.Strings(keys)
	return keys
}

func (f *fixture) version(t *testing.T, key string) string {
	t.Helper()
	object, err := f.s3.HeadObject(f.ctx, &s3.HeadObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	version := aws.ToString(object.VersionId)
	if version == "" || version == "null" || aws.ToInt64(object.ContentLength) == 0 {
		t.Fatalf("expected versioned, nonempty backup: %+v", object)
	}
	return version
}

func (f *fixture) assertBundle(t *testing.T, key string, perDatabase bool) {
	t.Helper()
	object, err := f.s3.GetObject(f.ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	data, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("backup is not a valid ZIP: %v", err)
	}
	files := make(map[string][]byte)
	for _, file := range reader.File {
		input, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(input)
		_ = input.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = content
	}
	for _, name := range []string{"data.archive.gz", "replica-set.json", "manifest.json", "config/mongod.conf", "config/keyFile"} {
		if len(files[name]) == 0 {
			t.Fatalf("bundle %s missing %s", key, name)
		}
	}
	if perDatabase && len(files["auth.archive.gz"]) == 0 {
		t.Fatal("per-database bundle is missing authentication archive")
	}
	if !json.Valid(files["manifest.json"]) || !json.Valid(files["replica-set.json"]) {
		t.Fatal("manifest or replica-set configuration is not valid JSON")
	}
	if string(files["config/mongod.conf"]) != testConfig {
		t.Fatal("bundled configuration differs from the mounted source")
	}
}

func authenticatedURI(port, replicaSet string) string {
	return "mongodb://root:" + testPassword + "@127.0.0.1:" + port + "/?replicaSet=" + replicaSet + "&authSource=admin"
}

func unusedPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
}

func requireBinary(t *testing.T, variable, fallback string) string {
	t.Helper()
	name := envOr(variable, fallback)
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("integration requires %s (%s): %v", name, variable, err)
	}
	return path
}

func startProcess(t *testing.T, binary string, environment []string, args ...string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "process.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), environment...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = log.Close()
		if t.Failed() {
			if data, err := os.ReadFile(logPath); err == nil {
				if len(data) > 12000 {
					data = data[len(data)-12000:]
				}
				t.Logf("%s log:\n%s", filepath.Base(binary), data)
			}
		}
	})
}

func docker(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func dockerCleanup(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil && !strings.Contains(string(output), "No such container") {
		t.Logf("cleanup docker %v: %v\n%s", args, err, output)
	}
}

func waitFor(t *testing.T, ctx context.Context, name string, probe func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var lastErr error
	for {
		probeCtx, stop := context.WithTimeout(ctx, 4*time.Second)
		lastErr = probe(probeCtx)
		stop()
		if lastErr == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v; last probe: %v", name, ctx.Err(), lastErr)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
