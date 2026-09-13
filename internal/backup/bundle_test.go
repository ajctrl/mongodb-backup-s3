package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureBundle(t *testing.T, mode string, modify func(*Manifest, map[string][]byte)) string {
	t.Helper()
	contents := map[string][]byte{"data.archive.gz": []byte("fake dump"), "replica-set.json": []byte(`{"_id":"rs0"}`), "config/node1/mongod.conf": []byte("replication:\n  replSetName: rs0\n")}
	m := Manifest{FormatVersion: 1, Mode: mode, Oplog: mode == "full", StartedAt: time.Now().Add(-time.Minute), CompletedAt: time.Now(), ServerVersion: "8.0.0", FCV: "8.0", ToolsVersion: "mongodump version: 100.18.0", ConfigIncluded: true, Files: map[string]FileRecord{}}
	if mode == "per-database" {
		m.Database = "app"
		contents["auth.archive.gz"] = []byte("auth dump")
	}
	for name, b := range contents {
		h := sha256.Sum256(b)
		m.Files[name] = FileRecord{Size: int64(len(b)), SHA256: hex.EncodeToString(h[:])}
	}
	if modify != nil {
		modify(&m, contents)
	}
	file := filepath.Join(t.TempDir(), "backup.zip")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for name, b := range contents {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	w, err := z.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(w).Encode(m); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}
func TestBundleChecksInventoryHashesScopeBeforeUse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*Manifest, map[string][]byte)
	}{
		{"corruption", func(m *Manifest, c map[string][]byte) { c["data.archive.gz"] = []byte("bad data!") }},
		{"missing config", func(m *Manifest, c map[string][]byte) { delete(c, "config/node1/mongod.conf") }},
		{"missing auth", func(m *Manifest, c map[string][]byte) { delete(c, "auth.archive.gz") }},
		{"wrong mode", func(m *Manifest, c map[string][]byte) { m.Mode = "full" }},
		{"wrong db", func(m *Manifest, c map[string][]byte) { m.Database = "other" }},
		{"oplog mismatch", func(m *Manifest, c map[string][]byte) { m.Oplog = true }},
		{"path traversal", func(m *Manifest, c map[string][]byte) { c["config/../../escape"] = []byte("secret") }},
		{"unknown version", func(m *Manifest, c map[string][]byte) { m.FormatVersion = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := fixtureBundle(t, "per-database", tc.modify)
			z, err := zip.OpenReader(file)
			if err != nil {
				t.Fatal(err)
			}
			defer z.Close()
			if _, err := validateBundle(t.Context(), z, Config{BackupMode: "per-database", Database: "app"}); err == nil {
				t.Fatal("invalid bundle accepted")
			}
		})
	}
}
func TestExtractConfigPrivateAndNoOverwrite(t *testing.T) {
	z, err := zip.OpenReader(fixtureBundle(t, "full", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	if _, err := validateBundle(t.Context(), z, Config{BackupMode: "full"}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "extracted")
	if err := extractConfiguration(t.Context(), z, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "data.archive.gz")); !os.IsNotExist(err) {
		t.Fatal("data archive extracted as configuration")
	}
	file := filepath.Join(dest, "config/node1/mongod.conf")
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("config permissions not private")
	}
	if err := extractConfiguration(t.Context(), z, dest); err == nil {
		t.Fatal("existing destination overwritten")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := validateBundle(ctx, z, Config{BackupMode: "full"}); err == nil {
		t.Fatal("canceled verification accepted")
	}
}
func TestSecretWriterRedactsAcrossChunks(t *testing.T) {
	var out bytes.Buffer
	w := newSecretWriter(&out, Config{URI: "mongodb://user:secret@host/", Passphrase: "my-pass"})
	w.Write([]byte("error mongodb://user:sec"))
	w.Write([]byte("ret@host/ password=sec"))
	w.Write([]byte("ret my-pass\n"))
	w.Flush()
	if strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "my-pass") || strings.Contains(out.String(), "mongodb://") {
		t.Fatalf("credentials leaked: %s", out.String())
	}
	if !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatal("expected redaction marker")
	}
}
