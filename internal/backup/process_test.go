package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamingPathsDrainSlowOutputAfterProcessExit(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			a, _, _, _ := newAppTest(t, "full")
			a.Config.Passphrase = "unit gpg passphrase"
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			done := make(chan error, 1)
			go func() {
				err := a.withRunner(nil, func(r commandRunner) error {
					return produceEncrypted(ctx, writer, r, encrypted, func(ctx context.Context, out io.Writer) error {
						return r.runInput(ctx, nil, out, "sh", "-c", "printf x")
					})
				})
				writer.CloseWithError(err)
				done <- err
			}()
			// Exceeds the former five-second WaitDelay that truncated slow streams.
			select {
			case <-time.After(6 * time.Second):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			body, err := io.ReadAll(reader)
			if err != nil || string(body) != "x" {
				t.Fatalf("slow output was truncated: %q, %v", body, err)
			}
			if err := <-done; err != nil {
				t.Fatalf("completed process failed while draining output: %v", err)
			}
		})
	}
}

func TestToolCredentialsCannotEnterConfigBundle(t *testing.T) {
	a, _, _, _ := newAppTest(t, "full")
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "mongod.conf"), []byte("setting"), 0600); err != nil {
		t.Fatal(err)
	}
	a.Config.ConfigDir = dir
	a.Config.Passphrase = "unit gpg passphrase"
	err := a.withRunner(nil, func(r commandRunner) error {
		for _, name := range []string{"mongodump", "mongorestore", "gpg"} {
			cmd, closeFiles, err := r.command(t.Context(), name)
			if err != nil {
				return err
			}
			defer closeFiles()
			file := cmd.ExtraFiles[0]
			if _, err := os.Stat(file.Name()); !os.IsNotExist(err) {
				t.Fatalf("named credential file for %s", name)
			}
			secret, err := io.ReadAll(file)
			if err != nil || len(secret) == 0 {
				t.Fatalf("missing descriptor content for %s", name)
			}
			if name == "gpg" {
				if string(secret) != a.Config.Passphrase {
					t.Fatal("missing GPG passphrase")
				}
			} else {
				var uri string
				if json.Unmarshal([]byte(strings.TrimPrefix(string(secret), "uri: ")), &uri) != nil || uri != a.Config.URI {
					t.Fatalf("missing URI for %s", name)
				}
			}
		}
		return captureForTest(t.Context(), dir, func(name string, r io.Reader) error {
			data, err := io.ReadAll(r)
			if name != "config/mongod.conf" || string(data) != "setting" {
				t.Fatalf("unexpected config member: %q", name)
			}
			return err
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestChildCredentialEnvironment(t *testing.T) {
	t.Setenv("S3_SECRET_ACCESS_KEY", "s3-only")
	t.Setenv("AWS_SESSION_TOKEN", "mongo-aws")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "/test/token")
	for _, mechanism := range []string{"SCRAM-SHA-256", "MONGODB-AWS", "MONGODB-OIDC"} {
		a, _, _, _ := newAppTest(t, "per-database")
		a.Config.URI += "&authMechanism=" + mechanism
		err := a.withRunner(nil, func(r commandRunner) error {
			for _, name := range []string{"gpg", "mongodump", "mongorestore"} {
				cmd, closeFiles, err := r.command(t.Context(), name)
				if err != nil {
					return err
				}
				closeFiles()
				env := strings.Join(cmd.Env, "\n")
				wantAWS := name != "gpg" && mechanism == "MONGODB-AWS"
				wantTokenFile := name != "gpg" && (mechanism == "MONGODB-OIDC" || mechanism == "MONGODB-AWS")
				if strings.Contains(env, "S3_SECRET_ACCESS_KEY=") || strings.Contains(env, "AWS_SESSION_TOKEN=mongo-aws") != wantAWS ||
					strings.Contains(env, "AWS_WEB_IDENTITY_TOKEN_FILE=/test/token") != wantTokenFile {
					t.Fatalf("incorrect credential environment for %s using %s", name, mechanism)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
