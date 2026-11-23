//go:build integration

package integration_test

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

func TestDatabaseRemovedAfterSelection(t *testing.T) {
	ctx, cancel := fixtureContext(t, 3*time.Minute)
	defer cancel()
	f := newFixture(t, ctx)
	source := f.connect(t, f.sourceURI)
	f.seed(t, source)
	if err := source.Database("later").CreateCollection(ctx, "empty"); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"S3_PREFIX": "removed", "BACKUP_MODE": "per-database", "BACKUP_FILENAME_MODE": "fixed", "BACKUP_ALLOW_UNVERSIONED_OVERWRITE": "true", "MONGODB_DATABASE": "later"}
	f.run(t, env, "backup")
	const key = "removed/per-database/later/latest.zip"
	saved := f.version(t, key)
	delete(env, "MONGODB_DATABASE")
	env["MONGODB_DATABASES"] = `["app","later","analytics"]`
	cmd, _ := f.backupCommand(t, env, "backup")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(out)
	var dropped bool
	for scanner.Scan() {
		if !dropped && strings.Contains(scanner.Text(), `Creating and uploading backup: "app"`) {
			if err := source.Database("later").Drop(ctx); err != nil {
				t.Error(err)
			} else {
				dropped = true
			}
		}
	}
	if err := cmd.Wait(); err == nil || scanner.Err() != nil || !dropped {
		t.Fatalf("expected failure for removed DB: drop=%t, scan=%v, command=%v", dropped, scanner.Err(), err)
	}
	if f.version(t, key) != saved || len(f.keys(t, "removed/")) != 3 {
		t.Fatal("lost last good generation or failed to save remaining databases")
	}
}
