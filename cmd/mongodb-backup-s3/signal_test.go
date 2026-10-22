package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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
