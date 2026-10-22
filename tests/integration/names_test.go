//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestLinuxDatabaseNames(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	f := newFixture(t, ctx)
	source, target := f.connect(t, f.sourceURI), f.connect(t, f.targetURI)
	names := []string{"app", "legacy:db", "star*db", "angle<db>", "pipe|db", "question?db", "private", "archive", "selected,db"}
	for _, name := range append(append([]string{}, names...), "excluded:db", "private,archive") {
		if _, err := source.Database(name).Collection("probe").InsertOne(ctx, bson.M{
			"_id": 1, "value": name, "payload": []byte{0, 1, 255, 39, 92, 10},
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.run(t, map[string]string{
		"S3_PREFIX": "linux-names", "BACKUP_MODE": "per-database", "MONGODB_BACKUP_ALL": "true",
		"MONGODB_DATABASES_EXCLUDE": `["excluded:db","private,archive"]`, "BACKUP_KEEP_DAYS": "1",
	}, "backup")
	if keys := f.keys(t, "linux-names/"); len(keys) != len(names) {
		t.Fatalf("discovery/exclusion produced wrong number of backups: %v", keys)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			f.run(t, map[string]string{
				"S3_PREFIX": "linux-names", "BACKUP_MODE": "per-database", "MONGODB_DATABASE": name,
				"MONGODB_URI": f.targetURI, "MONGODB_RESTORE_DROP": "true",
			}, "restore")
			f.assertValue(t, target, name, name)
		})
	}
	for _, name := range []string{"excluded:db", "private,archive"} {
		count, err := target.Database(name).Collection("probe").CountDocuments(ctx, bson.M{})
		if err != nil || count != 0 {
			t.Fatalf("excluded database %q was restored: count=%d error=%v", name, count, err)
		}
	}
	f.run(t, map[string]string{
		"S3_PREFIX": "literal-selection", "BACKUP_MODE": "per-database", "BACKUP_FILENAME_MODE": "fixed",
		"MONGODB_DATABASES": `["selected,db","app"]`,
	}, "backup")
	if got := strings.Join(f.keys(t, "literal-selection/"), "\n"); got != "literal-selection/per-database/app/latest.zip\nliteral-selection/per-database/selected%2Cdb/latest.zip" {
		t.Fatalf("JSON selection did not preserve literal database names: %s", got)
	}
}
