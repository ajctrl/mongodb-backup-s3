package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
