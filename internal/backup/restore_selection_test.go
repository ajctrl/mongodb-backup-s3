package backup

import (
	"archive/zip"
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
				return a.withBundle(t.Context(), nil, r, func(_ *zip.ReadCloser, m Manifest, _ string) error {
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
