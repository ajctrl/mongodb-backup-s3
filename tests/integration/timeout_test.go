//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Test the installed tools, including the actual image in CI: a mock cannot
// catch the dependency's milliseconds/seconds conversion regressions.
func TestDatabaseToolsTimeouts(t *testing.T) {
	for _, tool := range []string{"mongodump", "mongorestore"} {
		for _, option := range []string{"serverSelectionTimeoutMS", "connectTimeoutMS"} {
			for _, seconds := range []int{1, 2} {
				t.Run(fmt.Sprintf("%s/%s/%ds", tool, option, seconds), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
					defer cancel()
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					uri := fmt.Sprintf("mongodb://%s/?directConnection=true&%s=%d", listener.Addr(), option, seconds*1000)
					closed := make(chan time.Duration, 1)
					if option == "connectTimeoutMS" {
						uri += "&serverSelectionTimeoutMS=5000"
						go func() {
							conn, err := listener.Accept()
							if err != nil {
								return
							}
							defer conn.Close()
							listener.Close()
							stop := context.AfterFunc(ctx, func() { conn.Close() })
							defer stop()
							started := time.Now()
							// Accept the handshake but never respond. The tool must
							// close this connection at its configured connect timeout.
							_, _ = io.Copy(io.Discard, conn)
							closed <- time.Since(started)
						}()
					}
					// For server selection leave the listener open without accepting;
					// there is no MongoDB handshake or selectable server.
					args := []string{"--uri=" + uri, "--archive", "--gzip"}
					cmd := exec.CommandContext(ctx, tool, args...)
					if os.Getenv("MONGODB_NATIVE_INTEGRATION") != "1" {
						name := fmt.Sprintf("mongodb-timeout-%d", time.Now().UnixNano())
						t.Cleanup(func() { dockerCleanup(t, "rm", "-f", name) })
						args = append([]string{"run", "--rm", "--name", name, "--network", "host",
							envOr("BACKUP_TEST_IMAGE", "mongodb-backup-s3:integration"), tool}, args...)
						cmd = exec.CommandContext(ctx, "docker", args...)
					}
					started := time.Now()
					output, err := cmd.CombinedOutput()
					elapsed := time.Since(started)
					if ctx.Err() != nil || err == nil || !strings.Contains(string(output), "server selection error") {
						t.Fatalf("expected bounded connection failure: elapsed=%s error=%v output=%s", elapsed, err, output)
					}
					if option == "connectTimeoutMS" {
						select {
						case elapsed = <-closed:
						case <-ctx.Done():
							t.Fatal("tool did not close the stalled handshake")
						}
						// A selection timeout also closes the handshake after 5s;
						// keep the bound below that to verify connectTimeoutMS itself.
						if elapsed > time.Duration(seconds)*time.Second+time.Second {
							t.Fatalf("connect timeout was not honored: %s", elapsed)
						}
					}
					if elapsed < time.Duration(seconds)*time.Second-100*time.Millisecond || elapsed > 10*time.Second {
						t.Fatalf("%s=%d took %s", option, seconds*1000, elapsed)
					}
					t.Logf("%s=%d: %s", option, seconds*1000, elapsed)
				})
			}
		}
	}
}
