package backup

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLatestRestoreUsesManifestCompletionForS3TimestampTies(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			a, store, _, _ := newAppTest(t, "full")
			if encrypted {
				a.Config.Passphrase = "unit gpg passphrase"
			}
			base := a.now()
			for i, id := range []string{strings.Repeat("f", 32), strings.Repeat("0", 32)} {
				completed := base.Add(time.Duration(i) * 500 * time.Millisecond)
				a.now = func() time.Time { return completed }
				if err := a.Backup(t.Context()); err != nil {
					t.Fatal(err)
				}
				original := store.lastUploadedKey
				key := a.Config.timestampPrefix("") + base.Format(timestampLayout) + "-" + id + a.Config.fileType()
				store.objects[key], store.modified[key] = store.objects[original], base
				delete(store.objects, original)
			}
			// The newer manifest must win even though its random ID sorts first.
			if err := a.withRunner(nil, func(r commandRunner) error {
				return a.withBundle(t.Context(), nil, r, func(_ *zip.Reader, m Manifest) error {
					if !m.CompletedAt.Equal(base.Add(500 * time.Millisecond)) {
						t.Fatalf("restored older tied generation: %s", m.CompletedAt)
					}
					return nil
				})
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTiedRestoreNeverSkipsAnInvalidCandidate(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			a, store, _, log := newAppTest(t, "full")
			if encrypted {
				a.Config.Passphrase = "unit gpg passphrase"
			}
			for range 2 {
				if err := a.Backup(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			store.objects[store.lastUploadedKey] = alterAppTestBundle(t, store.objects[store.lastUploadedKey], "config/mongod.conf")
			if err := os.WriteFile(log, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.Restore(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "checksum") {
				t.Fatalf("invalid tied candidate did not prevent restore: %v", err)
			}
			if strings.Contains(readAppCommandLog(t, log), "mongorestore") {
				t.Fatal("an import started before all tied candidates were validated")
			}
		})
	}
}

func TestLinuxDatabaseDiscoveryExclusionAndRestoreScope(t *testing.T) {
	a, store, source, log := newAppTest(t, "per-database")
	source.names = []string{"admin", "app", "legacy:db", "literal*db", "excluded:db"}
	a.Config.Database, a.Config.BackupAll, a.Config.Exclude = "", "true", []string{"excluded:db", "admin"}
	if err := a.Backup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(store.objects) != 3 {
		t.Fatalf("Linux names stopped discovery or exclusion failed: objects=%d", len(store.objects))
	}
	if strings.Contains(readAppCommandLog(t, log), "--db=excluded:db") {
		t.Fatal("excluded database was dumped")
	}
	a.Config.BackupAll, a.Config.Exclude = "false", nil
	for _, name := range []string{"app", "legacy:db", "literal*db"} {
		a.Config.Database = name
		if err := os.WriteFile(log, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.Restore(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		pattern := name + ".*"
		if name == "literal*db" {
			pattern = `literal\*db.*`
		}
		if !strings.Contains(readAppCommandLog(t, log), "--nsInclude="+pattern+"\n") {
			t.Fatalf("restore did not limit import to literal DB name %q", name)
		}
		if !strings.Contains(store.downloadedKey, "/"+encodeComponent(name)+"/") {
			t.Fatalf("restore selected wrong DB: %s", filepath.Base(store.downloadedKey))
		}
	}
}

func TestDestructiveRestoreChecksVersionsBeforeImport(t *testing.T) {
	for _, tc := range []struct {
		mode string
		drop bool
	}{{"full", false}, {"full", true}, {"per-database", true}} {
		a, _, _, log := newAppTest(t, tc.mode)
		if err := a.Backup(t.Context()); err != nil {
			t.Fatal(err)
		}
		a.Config.RestoreDrop, a.Config.AllowUnknownSourceVersion = tc.drop, true
		for _, dst := range []Snapshot{{ServerVersion: "8.1.0", FCV: "8.0"}, {ServerVersion: "8.0.2", FCV: "7.0"}} {
			a.targetVersions = func(context.Context, Config) (Snapshot, error) { return dst, nil }
			restore := a.Restore
			if tc.mode == "per-database" {
				restore = a.RestoreAuth
			}
			if err := restore(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "version mismatch") {
				t.Fatalf("mode=%s drop=%t mismatch: %v", tc.mode, tc.drop, err)
			}
			if strings.Contains(readAppCommandLog(t, log), "mongorestore") {
				t.Fatal("import started on mismatch")
			}
		}
	}
}

func TestUnknownSourceStillRequiresTargetInspection(t *testing.T) {
	a, _, _, _ := newAppTest(t, "partial")
	a.Config.AllowUnknownSourceVersion = true
	a.targetVersions = func(context.Context, Config) (Snapshot, error) { return Snapshot{}, fmt.Errorf("target unavailable") }
	if err := a.checkRestoreVersions(t.Context(), Manifest{Mode: "partial"}); err == nil || !strings.Contains(err.Error(), "target unavailable") {
		t.Fatalf("missing target check: %v", err)
	}
}

func TestExtractRefusesUnusableDestinationBeforeListing(t *testing.T) {
	a, store, _, _ := newAppTest(t, "full")
	dir := t.TempDir()
	destinations := []string{dir, filepath.Join(dir, "absent", "output")}
	if os.Geteuid() != 0 {
		readOnly := t.TempDir()
		if err := os.Chmod(readOnly, 0555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(readOnly, 0700) })
		destinations = append(destinations, filepath.Join(readOnly, "output"))
	}
	for _, dest := range destinations {
		if err := a.ExtractConfig(t.Context(), dest, nil); err == nil || store.lists != 0 || store.downloadedKey != "" {
			t.Fatalf("destination %q: err=%v listings=%d", dest, err, store.lists)
		}
	}
}

func TestRestoreCARequiresTLSBeforeStorageAccess(t *testing.T) {
	a, store, _, _ := newAppTest(t, "per-database")
	a.Config.TLSCA = "absent"
	for _, restore := range []func(context.Context, []string) error{a.Restore, a.RestoreAuth} {
		if err := restore(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "requires TLS") || store.lists != 0 {
			t.Fatalf("CA preflight: %v listings=%d", err, store.lists)
		}
	}
}

func TestRestoreNeverLogsRawToolOutput(t *testing.T) {
	a, _, _, _ := newAppTest(t, "per-database")
	if err := a.Backup(t.Context()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mongorestore"), []byte("#!/bin/sh\necho private-document\necho private-document >&2\nexit 19\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	var output strings.Builder
	a.Out, a.Err = &output, &output
	for _, auth := range []bool{false, true} {
		a.Config.RestoreDrop = auth
		restore, stage := a.Restore, "apply data.archive.gz"
		if auth {
			restore, stage = a.RestoreAuth, "restore users and roles"
		}
		err := restore(t.Context(), nil)
		if err == nil || !strings.Contains(err.Error(), stage+": mongorestore failed: exit status 19") || strings.Contains(output.String()+err.Error(), "private-document") {
			t.Fatalf("auth=%t: err=%v output=%s", auth, err, output.String())
		}
	}
}

func TestUnknownSourceOverrideStillChecksAvailableFields(t *testing.T) {
	a, _, _, _ := newAppTest(t, "partial")
	a.Config.AllowUnknownSourceVersion = true
	for _, m := range []Manifest{{Mode: "partial", ServerVersion: "7.0.0"}, {Mode: "partial", FCV: "7.0"}} {
		if err := a.checkRestoreVersions(t.Context(), m); err == nil || !strings.Contains(err.Error(), "version mismatch") {
			t.Fatalf("skipped available source field: %v", err)
		}
	}
}
