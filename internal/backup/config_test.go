package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T, values map[string]string) Config {
	t.Helper()
	c, err := loadConfig(func(k string) (string, bool) { v, ok := values[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestConfigurationSelectionContracts(t *testing.T) {
	base := map[string]string{"S3_BUCKET": "bucket", "MONGODB_URI": "mongodb://backup:secret@localhost/?replicaSet=rs0&authSource=admin"}
	for _, tc := range []struct {
		name  string
		env   map[string]string
		valid bool
	}{
		{"full default", nil, true},
		{"full selection", map[string]string{"MONGODB_DATABASE": "app"}, false},
		{"full discovery", map[string]string{"MONGODB_BACKUP_ALL": "true"}, false},
		{"per DB", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASE": "app"}, true},
		{"multiple", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASES": "app, analytics"}, true},
		{"auto", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_BACKUP_ALL": "true", "MONGODB_DATABASES_EXCLUDE": "temp"}, true},
		{"no selector", map[string]string{"BACKUP_MODE": "per-database"}, false},
		{"auto conflict", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_BACKUP_ALL": "true", "MONGODB_DATABASE": "app"}, false},
		{"blank list member", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASES": "app,"}, false},
		{"internal DB", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASE": "admin"}, false},
		{"wildcard DB", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASE": "app*"}, false},
		{"bad keep days", map[string]string{"BACKUP_KEEP_DAYS": "-1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{}
			for k, v := range base {
				values[k] = v
			}
			for k, v := range tc.env {
				values[k] = v
			}
			c := testConfig(t, values)
			_, err := c.validateBackup()
			if (err == nil) != tc.valid {
				t.Fatalf("validation=%v valid=%v", err, tc.valid)
			}
		})
	}
}
func TestConnectionDoesNotChangeDumpScopeOrLeakSecrets(t *testing.T) {
	for _, uri := range []string{"mongodb://u:SECRET@host/app", "mongodb://u:SECRET@host/admin?authSource=admin", "mongodb://u:SECRET@host/?readPreference=secondary", "mongodb://u:SECRET@host/?bad=%zz", "mongodb://u:SECRET@host/#fragment"} {
		if err := validateURI(uri); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("URI validation unsafe: %v", err)
		}
	}
	for _, uri := range []string{"mongodb://u:p@a:27017,b:27017/?replicaSet=rs0&authSource=admin", "mongodb+srv://host/?authSource=admin"} {
		if err := validateURI(uri); err != nil {
			t.Fatal(err)
		}
	}
}
func TestURIFileAndExtractWithoutSource(t *testing.T) {
	file := filepath.Join(t.TempDir(), "uri")
	if err := os.WriteFile(file, []byte("mongodb://host/?authSource=admin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, map[string]string{"S3_BUCKET": "b", "MONGODB_URI_FILE": file})
	if c.URI != "mongodb://host/?authSource=admin" {
		t.Fatal("URI file newline not trimmed")
	}
	_, err := loadConfig(func(k string) (string, bool) {
		v, ok := map[string]string{"S3_BUCKET": "b", "MONGODB_URI": "mongodb://host", "MONGODB_URI_FILE": file}[k]
		return v, ok
	})
	if err == nil {
		t.Fatal("conflicting URI settings accepted")
	}
	c = testConfig(t, map[string]string{"S3_BUCKET": "b"})
	if c.requireConnection() == nil {
		t.Fatal("backup without URI accepted")
	}
}
func TestModeScopeAndRetentionMatching(t *testing.T) {
	stamp := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	full := Config{BackupMode: "full", Prefix: "backups/", FilenameMode: "timestamp"}
	db := full
	db.BackupMode = "per-database"
	if full.backupKey("", stamp) != "backups/full/2026-09-13T03:00:00.zip" || db.backupKey("a%b", stamp) != "backups/per-database/a%25b/2026-09-13T03:00:00.zip" {
		t.Fatal("incorrect scoped layout")
	}
	for _, key := range []string{db.backupKey("app", stamp), full.fixedKey(""), "backups/full/sub/2026-09-13T03:00:00.zip", "backups/full/notes.zip", "backups/full/2026-09-13T03:00:00.zip.bak"} {
		if full.isTimestampBackup("", key) {
			t.Fatalf("unsafe retention match %s", key)
		}
	}
	if !full.isTimestampBackup("", full.backupKey("", stamp)) {
		t.Fatal("timestamp not matched")
	}
}
