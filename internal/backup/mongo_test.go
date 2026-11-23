package backup

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/drivertest"
)

func TestFallbackPrimaryRequiresRequestedMajoritySnapshot(t *testing.T) {
	const host = "node:27017"
	hello := bson.D{
		{Key: "ok", Value: 1}, {Key: "me", Value: host}, {Key: "hosts", Value: bson.A{host}},
		{Key: "setName", Value: "rs"}, {Key: "isWritablePrimary", Value: true},
		{Key: "electionId", Value: bson.NewObjectID()},
		{Key: "lastWrite", Value: bson.M{
			"opTime":         bson.M{"ts": bson.Timestamp{T: 2}},
			"majorityOpTime": bson.M{"ts": bson.Timestamp{T: 1}},
		}},
	}
	for _, concern := range []string{"local", "majority"} {
		t.Run(concern, func(t *testing.T) {
			opts := options.Client()
			opts.Deployment = drivertest.NewMockDeployment(hello, hello)
			client, err := mongo.Connect(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Disconnect(context.Background())
			source := mongoSource{client: client, pref: readpref.Primary()}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			pinned, _, err := source.dumpMember(ctx, "mongodb://"+host+"/?readConcernLevel="+concern)
			if concern == "local" {
				if err != nil || pinned == "" {
					t.Fatalf("local primary selection: %q, %v", pinned, err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) || pinned != "" {
				t.Fatalf("majority primary selection: %q, %v", pinned, err)
			}
		})
	}
}

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

func TestMetadataCustomCAReplacesSystemRoots(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	custom := server.Certificate()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: custom.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"tlsInsecure", "sSlInsecure", "tlsAllowInvalidCertificates", "tlsAllowInvalidHostnames", "sslAllowInvalidCertificates", "sslAllowInvalidHostnames"} {
		c := Config{URI: "mongodb://localhost/?tls=true&" + option + "=true", TLSCA: ca}
		if err := c.requireConnection(); err == nil {
			t.Fatalf("custom CA allowed %s", option)
		}
	}
	other := filepath.Join(t.TempDir(), "other.pem")
	cert, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, cert, 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tlsCAFile", "sslCertificateAuthorityFile", "sslCAFile"} {
		cfg := Config{URI: "mongodb://localhost/?tls=true&" + key + "=" + other, TLSCA: ca}
		if err := cfg.requireRestoreConnection(); err == nil {
			t.Fatal("accepted conflicting CA files")
		}
	}
	opts, _, err := metadataOptions(Config{URI: "mongodb://localhost/?tls=true", TLSCA: ca})
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(custom)
	if !pool.Equal(opts.TLSConfig.RootCAs) {
		t.Fatal("custom CA configuration trusts additional roots")
	}
	system, err := x509.SystemCertPool()
	if err != nil || system.Equal(pool) {
		t.Fatal("test requires distinct system and custom trust roots")
	}
}

func TestDirectDumpURIPreservesConnectionOptions(t *testing.T) {
	for _, raw := range []string{
		"mongodb+srv://reader:p%40ss@example.invalid/?authSource=admin&srvMaxHosts=1&authMechanism=SCRAM-SHA-256",
		"mongodb://reader:p%40ss@one.invalid:27017,two.invalid:27017/?authSource=admin&TLS=true&directConnection=false&authMechanism=SCRAM-SHA-256",
	} {
		pinned, err := directDumpURI(raw, "[::1]:29000", "rs1")
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(pinned)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		password, _ := u.User.Password()
		if u.Scheme != "mongodb" || u.Host != "[::1]:29000" || password != "p@ss" ||
			q.Get("authSource") != "admin" || q.Get("authMechanism") != "SCRAM-SHA-256" ||
			q.Get("replicaSet") != "rs1" || q.Get("directConnection") != "true" || q.Has("srvMaxHosts") ||
			(q.Get("tls") != "true" && q.Get("TLS") != "true") {
			t.Fatal("pinned connection lost authentication, TLS, or replica set settings")
		}
	}
}
