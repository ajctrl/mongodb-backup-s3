package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSignalHelper(t *testing.T) {
	if os.Getenv("MONGODB_TEST_SIGNAL_HELPER") != "1" {
		return
	}
	os.Args = []string{"mongodb-backup-s3", "run"}
	if dest := os.Getenv("MONGODB_TEST_EXTRACT_DEST"); dest != "" {
		os.Args = []string{"mongodb-backup-s3", "extract-config", dest, "2026-09-13T03:00:00-0123456789abcdef0123456789abcdef"}
	}
	os.Exit(run())
}

func TestSignalsWhileWaitingForSchedule(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalHelper$")
			// Deliberately isolated from developer / CI backup credentials and configuration.
			cmd.Env = []string{"MONGODB_TEST_SIGNAL_HELPER=1", "S3_BUCKET=test", "S3_REGION=us-east-1",
				"S3_ACCESS_KEY_ID=test", "S3_SECRET_ACCESS_KEY=test", "AWS_EC2_METADATA_DISABLED=true",
				"MONGODB_URI=mongodb://localhost/?authSource=admin", "SCHEDULE=@every 1h", "HOME=" + t.TempDir()}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			scanner := bufio.NewScanner(stdout)
			ready := false
			for scanner.Scan() {
				if strings.Contains(scanner.Text(), "Scheduling backups:") {
					ready = true
					break
				}
			}
			if !ready {
				t.Fatalf("scheduler did not become ready: %v", scanner.Err())
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 128+int(sig) {
				t.Fatalf("signal %v: error=%v stderr=%s", sig, err, stderr.String())
			}
		})
	}
}

func TestKilledDownloadLeavesNoBackupBytes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses procfs to observe download progress")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	part := bytes.Repeat([]byte("a"), 1<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4194304")
		w.Header().Set("ETag", `"partial-download"`)
		if _, err := w.Write(part); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalHelper$")
	cmd.Env = []string{"MONGODB_TEST_SIGNAL_HELPER=1", "MONGODB_TEST_EXTRACT_DEST=" + filepath.Join(dir, "out"),
		"S3_BUCKET=test", "S3_REGION=us-east-1", "S3_ACCESS_KEY_ID=test", "S3_SECRET_ACCESS_KEY=test",
		"AWS_EC2_METADATA_DISABLED=true", "S3_ENDPOINT=" + server.URL, "TMPDIR=" + dir, "HOME=" + dir}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	for {
		fds, err := filepath.Glob(fmt.Sprintf("/proc/%d/fd/*", cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		received := false
		for _, fd := range fds {
			target, err := os.Readlink(fd)
			if err != nil || !strings.HasPrefix(target, dir+string(os.PathSeparator)) {
				continue
			}
			st, err := os.Stat(fd)
			if err == nil && st.Mode().IsRegular() && st.Size() > 0 {
				received = true
				break
			}
		}
		if received {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("no backup bytes reached disk")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected killed download")
	}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			t.Errorf("download left file %q after SIGKILL", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
