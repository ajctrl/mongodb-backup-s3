package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCaptureConfigNestedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "node1"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"rs.conf": "replica set", "node1/mongod.conf": "mongod", "node1/keyfile": "secret"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]string{}
	err := captureConfig(context.Background(), dir, func(name string, r io.Reader) error {
		data, err := io.ReadAll(r)
		got[name] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"config/rs.conf": "replica set", "config/node1/mongod.conf": "mongod", "config/node1/keyfile": "secret"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("captured %v, want %v", got, want)
	}
}

func TestCaptureConfigDirectoryValidation(t *testing.T) {
	empty := t.TempDir()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{"disabled", "", false},
		{"missing", filepath.Join(empty, "missing"), true},
		{"empty", empty, true},
		{"regular file", file, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := captureConfig(context.Background(), tc.dir, func(string, io.Reader) error {
				t.Fatal("unexpected file capture")
				return nil
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestCaptureConfigDetectsEarlierFileChange(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.conf", "b.conf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("configuration"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	err := captureConfig(context.Background(), dir, func(name string, r io.Reader) error {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return err
		}
		if name == "config/b.conf" {
			return os.WriteFile(filepath.Join(dir, "a.conf"), []byte("changed"), 0600)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("error = %v, want config change failure", err)
	}
}

func TestCaptureConfigDetectsSameLengthRewrite(t *testing.T) {
	for _, changeAt := range []string{"config/a.conf", "config/b.conf"} {
		t.Run(changeAt, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a.conf")
			for _, name := range []string{"a.conf", "b.conf"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			err = captureConfig(t.Context(), dir, func(name string, r io.Reader) error {
				if _, err := io.Copy(io.Discard, r); err != nil {
					return err
				}
				if name != changeAt {
					return nil
				}
				if err := os.WriteFile(path, []byte("new"), 0600); err != nil {
					return err
				}
				// Reproduce unchanged metadata deterministically, including when
				// an earlier file changes while a later file is being captured.
				return os.Chtimes(path, before.ModTime(), before.ModTime())
			})
			if err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("error = %v, want same-length rewrite failure", err)
			}
		})
	}
}

func TestCaptureConfigDetectsRootReplacement(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink", "missing"} {
		t.Run(replacement, func(t *testing.T) {
			parent := t.TempDir()
			dir, previous := filepath.Join(parent, "config"), filepath.Join(parent, "previous")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "mongod.conf"), []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			err := captureConfig(t.Context(), dir, func(_ string, r io.Reader) error {
				if _, err := io.Copy(io.Discard, r); err != nil {
					return err
				}
				if err := os.Rename(dir, previous); err != nil {
					return err
				}
				switch replacement {
				case "symlink":
					return os.Symlink(previous, dir)
				case "directory":
					if err := os.Mkdir(dir, 0700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(dir, "mongod.conf"), []byte("new"), 0600)
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "MONGODB_CONFIG_DIR changed") {
				t.Fatalf("error = %v, want root replacement failure", err)
			}
		})
	}
}

func TestCaptureConfigRejectsSymlinks(t *testing.T) {
	for _, rootLink := range []bool{false, true} {
		t.Run(map[bool]string{false: "nested", true: "root"}[rootLink], func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link")
			if err := os.Symlink(outside, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if rootLink {
				dir = link
			}
			err := captureConfig(context.Background(), dir, func(string, io.Reader) error {
				t.Fatal("symlink content must not be captured")
				return nil
			})
			if err == nil {
				t.Fatal("accepted symlink")
			}
		})
	}
}

func TestCaptureConfigCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keyfile"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := captureConfig(ctx, dir, func(_ string, r io.Reader) error {
		cancel()
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	err = captureConfig(ctx, dir, func(string, io.Reader) error {
		t.Fatal("capture ran after cancellation")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestCaptureConfigDetectsChangesAndIncompleteReads(t *testing.T) {
	for _, mutation := range []string{"rewrite", "append", "addition", "incomplete read"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mongod.conf")
			if err := os.WriteFile(path, []byte("original configuration"), 0600); err != nil {
				t.Fatal(err)
			}
			err := captureConfig(context.Background(), dir, func(_ string, r io.Reader) error {
				if mutation == "incomplete read" {
					return nil
				}
				if _, err := io.Copy(io.Discard, r); err != nil {
					return err
				}
				switch mutation {
				case "rewrite":
					return os.WriteFile(path, []byte("changed"), 0600)
				case "append":
					file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						return err
					}
					defer file.Close()
					_, err = file.WriteString("appended")
					return err
				case "addition":
					return os.WriteFile(filepath.Join(dir, "new-config"), []byte("new"), 0600)
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("error = %v, want config change failure", err)
			}
		})
	}
}

func TestCaptureConfigPropagatesWriterError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keyfile"), []byte("do-not-log-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("upload failed")
	err := captureConfig(context.Background(), dir, func(string, io.Reader) error { return wantErr })
	if !errors.Is(err, wantErr) || strings.Contains(err.Error(), "do-not-log-secret") {
		t.Fatalf("error = %v, want sanitized upload error", err)
	}
}

func TestCaptureConfigDetectsInventoryChangesWithoutNewModTime(t *testing.T) {
	for _, mutation := range []string{"add file", "add directory", "remove directory"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			nested := filepath.Join(dir, "nested")
			if err := os.Mkdir(nested, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(nested, "mongod.conf"), []byte("config"), 0600); err != nil {
				t.Fatal(err)
			}
			empty := filepath.Join(nested, "empty")
			if err := os.Mkdir(empty, 0700); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(nested)
			if err != nil {
				t.Fatal(err)
			}
			err = captureConfig(t.Context(), dir, func(_ string, r io.Reader) error {
				if _, err := io.Copy(io.Discard, r); err != nil {
					return err
				}
				var err error
				switch mutation {
				case "add file":
					err = os.WriteFile(filepath.Join(nested, "new.conf"), []byte("new"), 0600)
				case "add directory":
					err = os.Mkdir(filepath.Join(nested, "new"), 0700)
				case "remove directory":
					err = os.Remove(empty)
				}
				if err != nil {
					return err
				}
				// Model multiple changes within a single filesystem clock tick.
				return os.Chtimes(nested, before.ModTime(), before.ModTime())
			})
			if err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("error = %v, want config inventory change failure", err)
			}
		})
	}
}
