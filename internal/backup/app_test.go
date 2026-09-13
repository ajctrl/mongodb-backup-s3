package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ajctrl/mongodb-backup-s3/internal/storage"
)

type appTestSource struct {
	names       []string
	inspections int
	failAfter   int
	closed      bool
}

func (s *appTestSource) Inspect(context.Context) (Snapshot, error) {
	s.inspections++
	if s.failAfter > 0 && s.inspections >= s.failAfter {
		return Snapshot{}, errors.New("metadata inspection failed")
	}
	return Snapshot{ReplicaSet: []byte(`{"_id":"rs0","members":[{"_id":0,"host":"mongo:27017"}]}`), ServerVersion: "8.0.0", FCV: "8.0"}, nil
}
func (s *appTestSource) Databases(context.Context) ([]string, error) { return s.names, nil }
func (s *appTestSource) Close(context.Context) error                 { s.closed = true; return nil }

type appTestStore struct {
	objects       map[string][]byte
	modified      map[string]time.Time
	deleted       []string
	uploads       int
	lists         int
	versions      bool
	uploadErr     error
	downloadErr   error
	downloadedKey string
	versionID     string
}

func (s *appTestStore) VersioningEnabled(context.Context) (bool, error) { return s.versions, nil }
func (s *appTestStore) Upload(ctx context.Context, key string, body io.ReadCloser) error {
	s.uploads++
	defer body.Close()
	if s.uploadErr != nil {
		return s.uploadErr
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Model S3 multipart completion: failed streams never publish their bytes.
	s.objects[key] = b
	s.modified[key] = time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	return nil
}
func (s *appTestStore) Download(_ context.Context, key, version, path string) error {
	s.downloadedKey, s.versionID = key, version
	if s.downloadErr != nil {
		_ = os.WriteFile(path, []byte("partial download"), 0600)
		return s.downloadErr
	}
	b, ok := s.objects[key]
	if !ok {
		return fmt.Errorf("missing object %s", key)
	}
	return os.WriteFile(path, b, 0600)
}
func (s *appTestStore) List(_ context.Context, prefix string) ([]storage.Object, error) {
	s.lists++
	var result []storage.Object
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			result = append(result, storage.Object{Key: key, LastModified: s.modified[key]})
		}
	}
	return result, nil
}
func (s *appTestStore) Delete(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	delete(s.objects, key)
	return nil
}

// Fake command processes exercise the actual process runner, pipes, bundle
// producer, download staging and restore sequencing without needing MongoDB.
func newAppTest(t *testing.T, mode string) (*App, *appTestStore, *appTestSource, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "commands")
	for name, script := range map[string]string{
		"mongodump": `#!/bin/sh
set -eu
printf 'mongodump\n' >> "$TEST_COMMAND_LOG"
printf '%s\n' "$@" >> "$TEST_COMMAND_LOG"
auth=no
for arg in "$@"; do
  if [ "$arg" = --version ]; then printf 'mongodump version: 100.18.0\n'; exit 0; fi
  if [ "$arg" = --db=admin ]; then auth=yes; fi
done
printf 'archive bytes for %s' "$auth"
if [ "${TEST_DUMP_FAILURE:-}" = data ] && [ "$auth" = no ]; then exec 1>&-; exit 17; fi
if [ "${TEST_DUMP_FAILURE:-}" = auth ] && [ "$auth" = yes ]; then exec 1>&-; exit 18; fi
`,
		"mongorestore": `#!/bin/sh
set -eu
printf 'mongorestore\n' >> "$TEST_COMMAND_LOG"
printf '%s\n' "$@" >> "$TEST_COMMAND_LOG"
cat > "$TEST_RESTORE_INPUT"
if [ "${TEST_RESTORE_FAILURE:-}" = yes ]; then exit 19; fi
`,
		// This deliberately does not encrypt. It tests the real pipe, descriptor
		// and failure protocol; cryptographic round trips belong to integration.
		"gpg": `#!/bin/sh
set -eu
printf 'gpg\n' >> "$TEST_COMMAND_LOG"
printf '%s\n' "$@" >> "$TEST_COMMAND_LOG"
test "$1" = --passphrase-fd
fd="$2"
IFS= read -r secret < "/dev/fd/$fd" || :
test "$secret" = 'unit gpg passphrase'
test -z "${PASSPHRASE+x}"
test -z "${MONGODB_URI+x}"
decrypt=no
last=''
for arg in "$@"; do
  if [ "$arg" = --decrypt ]; then decrypt=yes; fi
  last="$arg"
done
if [ "$decrypt" = yes ]; then
  cat "$last"
  if [ "${TEST_GPG_FAILURE:-}" = decrypt ]; then exec 1>&-; exit 21; fi
else
  cat
  if [ "${TEST_GPG_FAILURE:-}" = encrypt ]; then exec 1>&-; exit 20; fi
fi
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_COMMAND_LOG", log)
	t.Setenv("TEST_RESTORE_INPUT", filepath.Join(dir, "restore-input"))
	t.Setenv("TEST_DUMP_FAILURE", "")
	t.Setenv("TEST_RESTORE_FAILURE", "")
	t.Setenv("TEST_GPG_FAILURE", "")
	configDir := filepath.Join(dir, "config")
	if err := os.Mkdir(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "mongod.conf"), []byte("replication:\n  replSetName: rs0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{
		URI:            "mongodb://backup:unit-secret@mongo:27017/?replicaSet=rs0&authSource=admin",
		ReadPreference: "primary", ConfigDir: configDir, BackupMode: mode, BackupAll: "false",
		Prefix: "unit", FilenameMode: "timestamp", ParallelCollections: 4,
	}
	if mode == "per-database" {
		c.Database = "app"
	}
	store := &appTestStore{objects: map[string][]byte{}, modified: map[string]time.Time{}, versions: true}
	source := &appTestSource{names: []string{"admin", "app", "analytics", "config", "local", "excluded"}}
	a := New(c, store, io.Discard, io.Discard)
	a.lockPath = filepath.Join(dir, "lock")
	a.now = func() time.Time { return time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC) }
	a.newSource = func(context.Context, Config) (Source, error) { return source, nil }
	return a, store, source, log
}

func readAppCommandLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBackupAndRestoreToolModes(t *testing.T) {
	for _, mode := range []string{"full", "per-database"} {
		t.Run(mode, func(t *testing.T) {
			a, store, source, log := newAppTest(t, mode)
			if err := a.Backup(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !source.closed || source.inspections != 2 || len(store.objects) != 1 {
				t.Fatalf("backup did not complete both metadata checks: source=%+v objects=%d", source, len(store.objects))
			}
			calls := readAppCommandLog(t, log)
			for _, flag := range []string{"--archive\n", "--gzip\n", "--readPreference=primary\n", "--numParallelCollections=4\n"} {
				if !strings.Contains(calls, flag) {
					t.Fatalf("missing dump flag %q in %s", flag, calls)
				}
			}
			if strings.Contains(calls, "unit-secret") || strings.Contains(calls, a.Config.URI) {
				t.Fatal("MongoDB URI was exposed in child process arguments")
			}
			if mode == "full" {
				if !strings.Contains(calls, "--oplog\n") || strings.Contains(calls, "--db=") {
					t.Fatalf("full dump must use oplog without database filter: %s", calls)
				}
			} else if strings.Contains(calls, "--oplog") || !strings.Contains(calls, "--db=app\n") || !strings.Contains(calls, "--db=admin\n") {
				t.Fatalf("per-database backup must separately capture data and all auth without oplog: %s", calls)
			}
			if err := os.WriteFile(log, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.Restore(t.Context(), []string{"2026-09-13T03:00:00"}); err != nil {
				t.Fatal(err)
			}
			calls = readAppCommandLog(t, log)
			for _, flag := range []string{"mongorestore\n", "--archive\n", "--gzip\n", "--stopOnError\n"} {
				if !strings.Contains(calls, flag) {
					t.Fatalf("missing restore flag %q in %s", flag, calls)
				}
			}
			if strings.Contains(calls, "--drop\n") {
				t.Fatal("restore unexpectedly enabled destructive drop by default")
			}
			input, err := os.ReadFile(os.Getenv("TEST_RESTORE_INPUT"))
			if err != nil || string(input) != "archive bytes for no" {
				t.Fatalf("data restore received the wrong archive on stdin: %q error=%v", input, err)
			}
			if mode == "full" {
				if !strings.Contains(calls, "--oplogReplay\n") || strings.Contains(calls, "--nsInclude=") {
					t.Fatalf("full restore flags: %s", calls)
				}
			} else if !strings.Contains(calls, "--nsInclude=app.*\n") || strings.Contains(calls, "--oplogReplay") || strings.Contains(calls, "auth.archive.gz") {
				t.Fatalf("per-database restore must import only selected data: %s", calls)
			}
		})
	}
}

func TestRestoreDropReconnectsAfterAuth(t *testing.T) {
	for _, mode := range []string{"full", "per-database"} {
		for _, authFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/authFails=%t", mode, authFails), func(t *testing.T) {
				a, _, _, log := newAppTest(t, mode)
				if err := a.Backup(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(log, nil, 0600); err != nil {
					t.Fatal(err)
				}
				a.Config.RestoreDrop = true
				if authFails {
					t.Setenv("TEST_RESTORE_FAILURE", "yes")
				}
				restore := a.Restore
				if mode == "per-database" {
					restore = a.RestoreAuth
				}
				err := restore(t.Context(), []string{"2026-09-13T03:00:00"})
				if (err != nil) != authFails {
					t.Fatalf("restore error=%v, want failure=%t", err, authFails)
				}
				calls := strings.Split(readAppCommandLog(t, log), "mongorestore\n")[1:]
				wantCalls := 2
				if authFails {
					wantCalls = 1
				}
				if len(calls) != wantCalls {
					t.Fatalf("got %d restore processes, want %d: %v", len(calls), wantCalls, calls)
				}
				for _, flag := range []string{"--drop\n", "--nsInclude=admin.system.users\n", "--nsInclude=admin.system.roles\n", "--nsInclude=admin.system.version\n"} {
					if !strings.Contains(calls[0], flag) {
						t.Fatalf("auth preparation missing %q: %s", flag, calls[0])
					}
				}
				if strings.Contains(calls[0], "--oplogReplay") {
					t.Fatal("auth preparation must not replay the oplog")
				}
				if !authFails {
					if !strings.Contains(calls[1], "--drop\n") {
						t.Fatal("restore lost drop option")
					}
					if mode == "full" && (!strings.Contains(calls[1], "--oplogReplay\n") || strings.Contains(calls[1], "--nsInclude")) {
						t.Fatalf("full restore must replay the oplog without namespace filters: %s", calls[1])
					}
					if mode == "per-database" && (!strings.Contains(calls[1], "--nsInclude=admin.*\n") || strings.Contains(calls[1], "--oplogReplay")) {
						t.Fatalf("auth restore scope: %s", calls[1])
					}
					wantInput := "archive bytes for no"
					if mode == "per-database" {
						wantInput = "archive bytes for yes"
					}
					input, err := os.ReadFile(os.Getenv("TEST_RESTORE_INPUT"))
					if err != nil || string(input) != wantInput {
						t.Fatalf("second restore did not receive the reopened archive: %q error=%v", input, err)
					}
				}
			})
		}
	}
}

func TestFailedBackupNeverPublishesOrRunsRetention(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, failure := range []string{"data", "auth", "metadata", "config", "config-filename", "upload"} {
			t.Run(fmt.Sprintf("%s/encrypted=%t", failure, encrypted), func(t *testing.T) {
				a, store, source, _ := newAppTest(t, "per-database")
				if encrypted {
					a.Config.Passphrase = "unit gpg passphrase"
				}
				a.Config.KeepDays = "7"
				oldKey := "unit/per-database/app/2026-08-01T00:00:00" + a.Config.fileType()
				store.objects[oldKey] = []byte("previous successful backup")
				store.modified[oldKey] = a.now().Add(-30 * 24 * time.Hour)
				switch failure {
				case "data", "auth":
					t.Setenv("TEST_DUMP_FAILURE", failure)
				case "metadata":
					source.failAfter = 2 // Data and authentication have already streamed.
				case "config":
					a.Config.ConfigDir = filepath.Join(t.TempDir(), "missing")
				case "config-filename":
					if err := os.WriteFile(filepath.Join(a.Config.ConfigDir, "bad-\xff.conf"), []byte("configuration"), 0600); err != nil {
						t.Fatal(err)
					}
				case "upload":
					store.uploadErr = errors.New("S3 upload failed")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if err := a.Backup(ctx); err == nil {
					t.Fatal("backup unexpectedly succeeded")
				} else if failure == "config-filename" && !strings.Contains(err.Error(), "unsupported backup member name") {
					t.Fatalf("unexpected filename error: %v", err)
				}
				if ctx.Err() != nil {
					t.Fatal("failed upload/producer did not promptly cancel its counterpart")
				}
				if len(store.objects) != 1 || string(store.objects[oldKey]) != "previous successful backup" || len(store.deleted) != 0 || store.lists != 0 {
					t.Fatalf("failure published a bundle or ran retention: keys=%v deleted=%v lists=%d", reflect.ValueOf(store.objects).MapKeys(), store.deleted, store.lists)
				}
			})
		}
	}
}

func TestUnicodeConfigurationFilenamesRoundTrip(t *testing.T) {
	a, _, _, _ := newAppTest(t, "full")
	names := []string{"設定.conf", "replacement-\ufffd.conf"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(a.Config.ConfigDir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Backup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.Restore(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "recovered")
	if err := a.ExtractConfig(t.Context(), destination, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(destination, "config", name))
		if err != nil || string(content) != name {
			t.Fatalf("configuration filename/content did not survive backup: name=%q content=%q error=%v", name, content, err)
		}
	}
}

func TestSuccessfulRetentionIsScopedToSelectedModeAndDatabase(t *testing.T) {
	a, store, _, _ := newAppTest(t, "per-database")
	a.Config.KeepDays = "7"
	old := "unit/per-database/app/2026-08-01T00:00:00.zip"
	for _, key := range []string{
		old,
		"unit/per-database/analytics/2026-08-01T00:00:00.zip",
		"unit/full/2026-08-01T00:00:00.zip",
		"unit/per-database/app/latest.zip",
		"unit/per-database/app/unrelated.zip",
	} {
		store.objects[key] = []byte("old object")
		store.modified[key] = a.now().Add(-30 * 24 * time.Hour)
	}
	if err := a.Backup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.deleted, []string{old}) {
		t.Fatalf("retention deleted outside the selected timestamp namespace: %v", store.deleted)
	}
}

func TestDatabaseDiscoveryAndSelection(t *testing.T) {
	a, store, _, log := newAppTest(t, "per-database")
	a.Config.Database, a.Config.BackupAll, a.Config.Exclude = "", "true", "excluded"
	if err := a.Backup(t.Context()); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(store.objects))
	for key := range store.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{"unit/per-database/analytics/2026-09-13T03:00:00.zip", "unit/per-database/app/2026-09-13T03:00:00.zip"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("discovery selected %v, want %v", keys, want)
	}
	if calls := readAppCommandLog(t, log); strings.Contains(calls, "--db=excluded") || strings.Contains(calls, "--db=local") || strings.Contains(calls, "--db=config") {
		t.Fatalf("excluded/internal database captured as ordinary data: %s", calls)
	}
}

func TestMissingExplicitDatabaseFailsBeforeAnyUpload(t *testing.T) {
	for _, selection := range []string{"missing", "app,missing"} {
		t.Run(selection, func(t *testing.T) {
			a, store, source, log := newAppTest(t, "per-database")
			if strings.Contains(selection, ",") {
				a.Config.Database, a.Config.Databases = "", selection
			} else {
				a.Config.Database = selection
			}
			err := a.Backup(t.Context())
			if err == nil || !strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("missing database did not fail clearly: %v", err)
			}
			if store.uploads != 0 || len(store.objects) != 0 || source.inspections != 0 {
				t.Fatalf("missing database still initiated a backup: uploads=%d objects=%d inspections=%d", store.uploads, len(store.objects), source.inspections)
			}
			if calls := readAppCommandLog(t, log); calls != "" {
				t.Fatalf("database validation unexpectedly launched dump tools: %s", calls)
			}
			if !source.closed {
				t.Fatal("missing database error did not release source connection")
			}
		})
	}
}

func TestExplicitAuthenticationRestoreAndVersionSelection(t *testing.T) {
	a, store, _, log := newAppTest(t, "per-database")
	a.Config.FilenameMode = "fixed"
	if err := a.Backup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, nil, 0600); err != nil {
		t.Fatal(err)
	}
	a.Config.RestoreDrop = true
	if err := a.RestoreAuth(t.Context(), []string{"--version-id", "older-version"}); err != nil {
		t.Fatal(err)
	}
	if store.versionID != "older-version" || store.downloadedKey != "unit/per-database/app/latest.zip" {
		t.Fatalf("restore selected wrong object/version: %q %q", store.downloadedKey, store.versionID)
	}
	calls := readAppCommandLog(t, log)
	for _, flag := range []string{"--archive\n", "--nsInclude=admin.*\n", "--drop\n", "--stopOnError\n"} {
		if !strings.Contains(calls, flag) {
			t.Fatalf("authentication restore missing %q: %s", flag, calls)
		}
	}
	if strings.Contains(calls, "--oplogReplay") || strings.Contains(calls, "--nsInclude=app.*") {
		t.Fatalf("authentication restore used data/oplog filters: %s", calls)
	}
	input, err := os.ReadFile(os.Getenv("TEST_RESTORE_INPUT"))
	if err != nil || string(input) != "archive bytes for yes" {
		t.Fatalf("authentication restore received the wrong archive on stdin: %q error=%v", input, err)
	}
}

func TestRestoreFailureNeverStartsDatabaseImport(t *testing.T) {
	for _, failure := range []string{"download", "invalid-zip", "checksum", "mode", "selection"} {
		t.Run(failure, func(t *testing.T) {
			a, store, _, log := newAppTest(t, "per-database")
			if err := a.Backup(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(log, nil, 0600); err != nil {
				t.Fatal(err)
			}
			key := a.Config.backupKey("app", a.now())
			switch failure {
			case "download":
				store.downloadErr = errors.New("download interrupted")
			case "invalid-zip":
				store.objects[key] = []byte("not a complete ZIP archive")
			case "checksum":
				store.objects[key] = alterAppTestBundle(t, store.objects[key], "config/mongod.conf")
			case "mode":
				a.Config.BackupMode, a.Config.Database = "full", ""
				store.objects[a.Config.backupKey("", a.now())] = store.objects[key]
			case "selection":
				a.Config.Databases = "app,analytics"
			}
			if err := a.Restore(t.Context(), nil); err == nil {
				t.Fatal("restore unexpectedly accepted incomplete or mismatched backup")
			}
			if calls := readAppCommandLog(t, log); calls != "" {
				t.Fatalf("restore started external tools before validation completed: %s", calls)
			}
		})
	}
}

func TestGPGPipelineDescriptorsAndExtractConfiguration(t *testing.T) {
	a, store, _, log := newAppTest(t, "per-database")
	a.Config.Passphrase = "unit gpg passphrase"
	t.Setenv("PASSPHRASE", a.Config.Passphrase)
	t.Setenv("MONGODB_URI", a.Config.URI)
	if err := a.Backup(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := a.Config.backupKey("app", a.now())
	if !strings.HasSuffix(key, ".zip.gpg") || len(store.objects[key]) == 0 {
		t.Fatalf("encrypted pipeline did not publish the encrypted key: %q", key)
	}
	if err := a.Restore(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "recovered")
	if err := a.ExtractConfig(t.Context(), destination, nil); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "config", "mongod.conf"))
	if err != nil || string(content) != "replication:\n  replSetName: rs0\n" {
		t.Fatalf("configuration did not survive encrypted pipeline: %q %v", content, err)
	}
	info, err := os.Stat(filepath.Join(destination, "config", "mongod.conf"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("extracted secret permissions are not 0600: info=%v error=%v", info, err)
	}
	calls := readAppCommandLog(t, log)
	if strings.Contains(calls, a.Config.Passphrase) || strings.Contains(calls, "unit-secret") {
		t.Fatal("credentials were exposed in child process arguments")
	}
	if strings.Count("\n"+calls, "\ngpg\n") != 3 || !strings.Contains(calls, "--passphrase-fd\n4\n") || !strings.Contains(calls, "--passphrase-fd\n3\n") {
		t.Fatalf("encryption and decryption did not pass independent secret descriptors: %s", calls)
	}
}

func TestFailedEncryptionNeverPublishesOrRunsRetention(t *testing.T) {
	a, store, _, _ := newAppTest(t, "per-database")
	a.Config.Passphrase, a.Config.KeepDays = "unit gpg passphrase", "7"
	oldKey := "unit/per-database/app/2026-08-01T00:00:00.zip.gpg"
	store.objects[oldKey] = []byte("previous encrypted backup")
	store.modified[oldKey] = a.now().Add(-30 * 24 * time.Hour)
	t.Setenv("TEST_GPG_FAILURE", "encrypt")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := a.Backup(ctx); err == nil {
		t.Fatal("failed encryption unexpectedly succeeded")
	}
	if ctx.Err() != nil {
		t.Fatal("encryption failure hung the producer or uploader")
	}
	if len(store.objects) != 1 || string(store.objects[oldKey]) != "previous encrypted backup" || store.lists != 0 || len(store.deleted) != 0 {
		t.Fatal("failed encryption published data or pruned a successful backup")
	}
}

func TestEarlyEncryptionFailureCancelsDumpAndReleasesLock(t *testing.T) {
	for _, producer := range []struct{ name, command string }{
		{"waiting for data", "exec sleep 30"},
		{"writing data", "exec yes archive-data"},
	} {
		t.Run(producer.name, func(t *testing.T) {
			a, store, _, log := newAppTest(t, "full")
			a.Config.Passphrase, a.Config.KeepDays = "unit gpg passphrase", "7"
			dir := filepath.Dir(log)
			t.Setenv("TEST_DUMP_STARTED", filepath.Join(dir, "dump-started"))
			for name, script := range map[string]string{
				"mongodump": `#!/bin/sh
set -eu
for arg in "$@"; do
  if [ "$arg" = --version ]; then printf 'mongodump version: 100.18.0\n'; exit 0; fi
done
: > "$TEST_DUMP_STARTED"
` + producer.command + "\n",
				"gpg": `#!/bin/sh
set -eu
while [ ! -f "$TEST_DUMP_STARTED" ]; do sleep 0.01; done
exit 23
`,
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := a.Backup(ctx)
			if ctx.Err() != nil {
				t.Fatal("early GPG failure required external cancellation to stop the dump")
			}
			if err == nil || !strings.Contains(err.Error(), "gpg failed: exit status 23") {
				t.Fatalf("early GPG failure was lost: %v", err)
			}
			if len(store.objects) != 0 || store.lists != 0 || len(store.deleted) != 0 {
				t.Fatal("early GPG failure published a backup or ran retention")
			}
			lock, err := acquireLock(a.lockPath)
			if err != nil {
				t.Fatalf("failed encryption left a process holding the backup lock: %v", err)
			}
			lock.Close()
		})
	}
}

func TestFailedDecryptionNeverStartsImport(t *testing.T) {
	a, _, _, log := newAppTest(t, "per-database")
	a.Config.Passphrase = "unit gpg passphrase"
	if err := a.Backup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_GPG_FAILURE", "decrypt")
	if err := a.Restore(t.Context(), nil); err == nil {
		t.Fatal("failed decryption unexpectedly succeeded")
	}
	if calls := readAppCommandLog(t, log); !strings.Contains(calls, "gpg\n") || strings.Contains(calls, "mongorestore\n") {
		t.Fatalf("import started despite failed decryption: %s", calls)
	}
}

// Rebuild with a valid ZIP CRC but leave the SHA-256 manifest unchanged. This
// proves even configuration that is not imported into MongoDB is verified first.
func alterAppTestBundle(t *testing.T, input []byte, member string) []byte {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(input), int64(len(input)))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	for _, file := range z.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if file.Name == member {
			data[0] ^= 1
		}
		h := &zip.FileHeader{Name: file.Name, Method: file.Method}
		h.SetMode(0600)
		dst, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dst.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
