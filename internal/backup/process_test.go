package backup

import (
	"context"
	"fmt"
	"io"
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
