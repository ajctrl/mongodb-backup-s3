package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

func TestRestoreWriteConcern(t *testing.T) {
	for _, tc := range []struct {
		query string
		valid bool
	}{
		{"", true}, {"w=1", true}, {"w=majority", true}, {"w=customTag", true},
		{"W=2&journal=true", true}, {"w=%2B1", true},
		{"w=0", false}, {"W=00", false}, {"%77=%30", false},
		{"w=-0", false}, {"w=%2B0", false},
		{"w=0&w=1", false}, {"w=1&W=0", false}, {"w=1&%77=1", false},
	} {
		for _, fromFile := range []bool{false, true} {
			t.Run(tc.query+fmt.Sprint("/file=", fromFile), func(t *testing.T) {
				uri := "mongodb://backup:SECRET@one:27017,two:27017/?authSource=admin&" + tc.query
				values := map[string]string{"S3_BUCKET": "bucket", "MONGODB_URI": uri}
				if fromFile {
					path := filepath.Join(t.TempDir(), "uri")
					if err := os.WriteFile(path, []byte(uri+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					delete(values, "MONGODB_URI")
					values["MONGODB_URI_FILE"] = path
				}
				c := testConfig(t, values)
				if _, err := c.validateBackup(); err != nil {
					t.Fatalf("restore-only restriction affected backup: %v", err)
				}
				err := c.requireRestoreConnection()
				if (err == nil) != tc.valid {
					t.Fatalf("valid=%v, error=%v", tc.valid, err)
				}
				if err != nil && strings.Contains(err.Error(), "SECRET") {
					t.Fatal("error leaked URI credentials")
				}
			})
		}
	}
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
		{"multiple", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASES": `["app","analytics"]`}, true},
		{"auto", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_BACKUP_ALL": "true", "MONGODB_DATABASES_EXCLUDE": `["temp"]`}, true},
		{"empty arrays in full", map[string]string{"MONGODB_DATABASES": `[]`, "MONGODB_DATABASES_EXCLUDE": `[]`}, true},
		{"empty array with single DB", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASE": "private,archive", "MONGODB_DATABASES": `[]`}, true},
		{"empty array without selector", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASES": `[]`}, false},
		{"empty array with discovery", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_BACKUP_ALL": "true", "MONGODB_DATABASES": `[]`}, true},
		{"no selector", map[string]string{"BACKUP_MODE": "per-database"}, false},
		{"auto conflict", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_BACKUP_ALL": "true", "MONGODB_DATABASE": "app"}, false},
		{"blank list member", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASES": `["app",""]`}, false},
		{"internal DB", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASE": "admin"}, false},
		{"literal star DB", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASE": "app*"}, true},
		{"Linux DB", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_DATABASE": "legacy:db"}, true},
		{"Linux exclusions", map[string]string{"BACKUP_MODE": "per-database", "MONGODB_BACKUP_ALL": "true", "MONGODB_DATABASES_EXCLUDE": `["legacy:db","app*","admin","private,archive"]`}, true},
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
			c, err := loadConfig(func(k string) (string, bool) { v, ok := values[k]; return v, ok })
			if err == nil {
				_, err = c.validateBackup()
			}
			if (err == nil) != tc.valid {
				t.Fatalf("validation=%v valid=%v", err, tc.valid)
			}
		})
	}
}

func TestDatabaseListJSON(t *testing.T) {
	for _, variable := range []string{"MONGODB_DATABASES", "MONGODB_DATABASES_EXCLUDE"} {
		for _, tc := range []struct {
			value   string
			want    []string
			invalid bool
		}{
			{"", nil, false},
			{`[]`, nil, false},
			{" \n [\"private,archive\",\"app\"] \t", []string{"private,archive", "app"}, false},
			{`["app","app"]`, []string{"app", "app"}, false},
			{`app,archive`, nil, true},
			{`"app"`, nil, true},
			{`null`, nil, true},
			{`{}`, nil, true},
			{`[null]`, nil, true},
			{`[1]`, nil, true},
			{`["app",""]`, nil, true},
			{`[" app"]`, nil, true},
			{`["app",]`, nil, true},
			{`["app"] []`, nil, true},
			{"[\"bad\xff\"]", nil, true},
		} {
			t.Run(variable+"/"+tc.value, func(t *testing.T) {
				values := map[string]string{"S3_BUCKET": "bucket", variable: tc.value}
				c, err := loadConfig(func(k string) (string, bool) { v, ok := values[k]; return v, ok })
				if tc.invalid {
					if err == nil || !strings.Contains(err.Error(), variable) {
						t.Fatalf("expected error naming %s, got %v", variable, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				got := c.Databases
				if variable == "MONGODB_DATABASES_EXCLUDE" {
					got = c.Exclude
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			})
		}
	}
}

func TestLinuxDatabaseNames(t *testing.T) {
	for _, name := range []string{"legacy:db", "star*db", "angle<db>", "pipe|db", "question?db", "日本語", strings.Repeat("a", 63)} {
		if err := validateDatabases([]string{name}); err != nil {
			t.Fatalf("valid Linux name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"", "a/b", `a\b`, "a.b", `a"b`, "a$b", "a b", "a\x00b", "bad\xff", strings.Repeat("a", 64)} {
		if err := validateDatabases([]string{name}); err == nil {
			t.Fatalf("invalid MongoDB name %q accepted", name)
		}
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

func TestURITimeoutPrecision(t *testing.T) {
	for _, key := range []string{"serverSelectionTimeoutMS", "connectTimeoutMS", "CONNECTTIMEOUTMS", "%73erverSelectionTimeoutMS"} {
		for _, value := range []string{"0", "1", "999", "1500", "-1000", "", "SECRET", "9223372037000", "1000&" + key + "=2000"} {
			uri := "mongodb://user:SECRET@host/?" + key + "=" + value
			for _, fromFile := range []bool{false, true} {
				values := map[string]string{"S3_BUCKET": "bucket", "MONGODB_URI": uri}
				if fromFile {
					path := filepath.Join(t.TempDir(), "uri")
					if err := os.WriteFile(path, []byte(uri+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					delete(values, "MONGODB_URI")
					values["MONGODB_URI_FILE"] = path
				}
				_, err := loadConfig(func(k string) (string, bool) { v, ok := values[k]; return v, ok })
				if err == nil || strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("unsafe timeout validation for key=%s value=%s file=%t: %v", key, value, fromFile, err)
				}
			}
		}
	}
	for _, query := range []string{"serverSelectionTimeoutMS=1000", "connectTimeoutMS=2000", "connectTimeoutMS=1000&serverSelectionTimeoutMS=2000"} {
		testConfig(t, map[string]string{"S3_BUCKET": "bucket", "MONGODB_URI": "mongodb://host/?" + query})
	}
	for _, query := range []string{"connectTimeoutMS=1000&CONNECTTIMEOUTMS=2000", "serverSelectionTimeoutMS=1000&SERVERSELECTIONTIMEOUTMS=2000"} {
		if err := validateURI("mongodb://host/?" + query); err == nil {
			t.Fatalf("case-insensitive duplicate accepted: %s", query)
		}
	}
}
func TestUnsupportedOperationTimeoutsRejected(t *testing.T) {
	for _, key := range []string{"socketTimeoutMS", "timeoutMS", "SOCKETTIMEOUTMS", "TimeOutMS", "socket%54imeoutMS", "%74imeoutMS"} {
		for _, value := range []string{"1000", "0", "", "SECRET", "1000&" + key + "=2000"} {
			uri := "mongodb://user:SECRET@host/?connectTimeoutMS=1000&" + key + "=" + value
			for _, fromFile := range []bool{false, true} {
				values := map[string]string{"S3_BUCKET": "bucket", "MONGODB_URI": uri}
				if fromFile {
					path := filepath.Join(t.TempDir(), "uri")
					if err := os.WriteFile(path, []byte(uri+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					delete(values, "MONGODB_URI")
					values["MONGODB_URI_FILE"] = path
				}
				_, err := loadConfig(func(k string) (string, bool) { v, ok := values[k]; return v, ok })
				if err == nil || !strings.Contains(err.Error(), "does not support") || strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("unsupported timeout key=%s file=%t was not safely rejected: %v", key, fromFile, err)
				}
			}
		}
	}
}

func TestModeScopeAndRetentionMatching(t *testing.T) {
	stamp := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	full := Config{BackupMode: "full", Prefix: "backups/", FilenameMode: "timestamp"}
	db := full
	db.BackupMode = "per-database"
	if !strings.HasPrefix(full.backupKey("", stamp), "backups/full/2026-09-13T03:00:00-") || !strings.HasPrefix(db.backupKey("a%b", stamp), "backups/per-database/a%25b/2026-09-13T03:00:00-") {
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

func TestGenerationValidation(t *testing.T) {
	const id = "-0123456789abcdef0123456789abcdef"
	for _, name := range []string{"2026-02-30T03:00:00" + id, "2026-09-13T25:00:00" + id, "2026-09-13T03:00:00", "2026-09-13T03:00:00" + id + ".zip", "../2026-09-13T03:00:00" + id} {
		if validGeneration(name) {
			t.Fatalf("invalid generation accepted: %q", name)
		}
	}
	if !validGeneration("2026-09-13T03:00:00" + id) {
		t.Fatal("valid generation rejected")
	}
}
