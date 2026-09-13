//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Exercise restore-auth without a preceding full restore: independently created
// root accounts have different identities even with the same name and password.
func TestAuthRestoreFreshDestination(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newFixture(t, ctx)
	source := f.connect(t, f.sourceURI)
	f.seed(t, source)
	audit := source.Database("admin").Collection("audit")
	if _, err := audit.InsertOne(ctx, bson.M{"_id": 1, "event": "backup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "event", Value: 1}}, Options: options.Index().SetName("unique_event").SetUnique(true),
	}); err != nil {
		t.Fatal(err)
	}
	target := f.connect(t, f.targetURI)
	if _, err := target.Database("app").Collection("probe").InsertOne(ctx, bson.M{
		"_id": 1, "value": "destination", "payload": []byte{0, 1, 255, 39, 92, 10},
	}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"S3_PREFIX": "auth-fresh", "BACKUP_MODE": "per-database",
		"MONGODB_DATABASE": "app", "BACKUP_FILENAME_MODE": "fixed",
	}
	f.run(t, env, "backup")
	env["MONGODB_URI"] = f.targetURI
	env["MONGODB_RESTORE_DROP"] = "true"
	f.run(t, env, "restore-auth")
	target = f.connect(t, f.targetURI)
	audit = target.Database("admin").Collection("audit")
	var restored struct{ Event string }
	if err := audit.FindOne(ctx, bson.M{"_id": 1}).Decode(&restored); err != nil || restored.Event != "backup" {
		t.Fatalf("restored admin data=%+v error=%v", restored, err)
	}
	if _, err := audit.InsertOne(ctx, bson.M{"_id": 2, "event": "backup"}); !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("restored admin unique index did not reject duplicate: %v", err)
	}
	f.assertAuth(t, "app", "appreader")
	f.assertAuth(t, "admin", "adminreader")
	f.assertValue(t, target, "app", "destination")
}
