package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

func TestMetadataOptions(t *testing.T) {
	c := Config{URI: "mongodb://backup:do-not-log@localhost/?replicaSet=rs0", ReadPreference: "secondary"}
	opts, pref, err := metadataOptions(c)
	if err != nil {
		t.Fatal(err)
	}
	if pref.Mode() != readpref.SecondaryMode || opts.ReadPreference.Mode() != readpref.SecondaryMode {
		t.Fatalf("unexpected read preference %v", pref)
	}
	if opts.Timeout == nil || *opts.Timeout != metadataTimeout || opts.ServerSelectionTimeout == nil || *opts.ServerSelectionTimeout != metadataTimeout {
		t.Fatal("metadata timeouts not configured")
	}
	if opts.Auth == nil || opts.Auth.Password != "do-not-log" {
		t.Fatal("connection authentication not preserved")
	}
}

func TestMetadataOptionsValidation(t *testing.T) {
	dir := t.TempDir()
	invalidCA := filepath.Join(dir, "invalid.pem")
	if err := os.WriteFile(invalidCA, []byte("do-not-log-certificate-contents"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]Config{
		"missing URI":        {},
		"invalid URI":        {URI: "mongodb://backup:do-not-log@@localhost"},
		"invalid preference": {URI: "mongodb://localhost", ReadPreference: "do-not-log"},
		"missing CA":         {URI: "mongodb://localhost/?tls=true", TLSCA: filepath.Join(dir, "missing.pem")},
		"invalid CA":         {URI: "mongodb://localhost/?tls=true", TLSCA: invalidCA},
		"CA without TLS":     {URI: "mongodb://localhost", TLSCA: invalidCA},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := metadataOptions(c)
			if err == nil {
				t.Fatal("invalid metadata options accepted")
			}
			if strings.Contains(err.Error(), "do-not-log") {
				t.Fatalf("error reveals credentials: %v", err)
			}
		})
	}
}

func TestMetadataErrorRedaction(t *testing.T) {
	for _, cause := range []error{
		errors.New("mongodb://user:do-not-log@localhost"),
		mongo.CommandError{Code: 13, Message: "do-not-log"},
		mongo.CommandError{Code: 18, Message: "do-not-log"},
		context.Canceled,
		context.DeadlineExceeded,
	} {
		err := metadataError("hello", cause)
		if strings.Contains(err.Error(), "do-not-log") {
			t.Fatalf("error reveals credentials: %v", err)
		}
		if errors.Is(cause, context.Canceled) && !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation lost")
		}
		if errors.Is(cause, context.DeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("timeout lost")
		}
	}
}

func TestReplicaSetCanonicalJSONRoundTrip(t *testing.T) {
	raw, err := bson.Marshal(bson.D{
		{Key: "_id", Value: "rs0"},
		{Key: "version", Value: int64(9007199254740993)},
		{Key: "settings", Value: bson.D{{Key: "replicaSetId", Value: bson.NewObjectID()}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := bson.MarshalExtJSON(bson.Raw(raw), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(encoded) || !strings.Contains(string(encoded), `"$numberLong":"9007199254740993"`) || !strings.Contains(string(encoded), `"$oid"`) {
		t.Fatalf("BSON types were not preserved: %s", encoded)
	}
}

func TestNewSourceAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewSource(ctx, Config{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation before connecting", err)
	}
}
