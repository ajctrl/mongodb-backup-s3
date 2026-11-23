//go:build integration

package integration_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The distroless image must retain runtime data and the GPG agent needed for
// encryption with its default HOME, without including a shell. The database round
// trips separately exercise the image as a non-root user with HOME=/tmp.
func TestImageRuntime(t *testing.T) {
	if os.Getenv("MONGODB_NATIVE_INTEGRATION") == "1" {
		t.Skip("requires the container image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	image := envOr("BACKUP_TEST_IMAGE", "mongodb-backup-s3:integration")
	for _, command := range [][]string{
		{"mongodb-backup-s3", "--help"},
		{"mongodump", "--version"},
		{"mongorestore", "--version"},
	} {
		docker(t, ctx, append([]string{"run", "--rm", image}, command...)...)
	}

	t.Run("filesystem", func(t *testing.T) {
		dir := t.TempDir()
		cidFile := filepath.Join(dir, "container.id")
		docker(t, ctx, "create", "--cidfile", cidFile, image)
		id, err := os.ReadFile(cidFile)
		if err != nil {
			t.Fatal(err)
		}
		container := strings.TrimSpace(string(id))
		defer dockerCleanup(t, "rm", "-v", container)
		export := filepath.Join(dir, "rootfs.tar")
		docker(t, ctx, "export", "--output", export, container)
		file, err := os.Open(export)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		required := map[string]bool{
			"etc/ssl/certs/ca-certificates.crt": false,
			"usr/share/zoneinfo/Asia/Tokyo":     false,
		}
		reader := tar.NewReader(file)
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			name := path.Clean(strings.TrimPrefix(header.Name, "/"))
			if _, ok := required[name]; ok {
				required[name] = header.Size > 0
			}
			switch path.Dir(name) {
			case ".", "bin", "sbin", "usr/bin", "usr/sbin", "usr/local/bin", "usr/local/sbin":
				switch path.Base(name) {
				case "sh", "bash", "dash", "ash", "zsh", "busybox", "apt", "apt-get", "dpkg", "apk", "yum", "dnf",
					"run.sh", "backup.sh", "restore.sh":
					t.Errorf("distroless image contains %s", name)
				}
			}
		}
		for name, found := range required {
			if !found {
				t.Errorf("runtime data missing or empty: %s", name)
			}
		}
	})

	t.Run("encryption", func(t *testing.T) {
		gpg := func(input []byte, operation string) []byte {
			t.Helper()
			cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", image,
				"gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "test-only",
				"--output", "-", operation)
			cmd.Stdin = bytes.NewReader(input)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("gpg %s: %v\n%s", operation, err, &stderr)
			}
			return output
		}
		plaintext := []byte("runtime encryption check")
		encrypted := gpg(plaintext, "--symmetric")
		if len(encrypted) == 0 || bytes.Equal(encrypted, plaintext) {
			t.Fatal("GPG did not encrypt the input")
		}
		if decrypted := gpg(encrypted, "--decrypt"); !bytes.Equal(decrypted, plaintext) {
			t.Fatalf("decrypted content = %q, want %q", decrypted, plaintext)
		}
	})
}
