package main

import (
	"os"
	"testing"
)

func TestHelpAndInvalidCommandsNeedNoConfiguration(t *testing.T) {
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	t.Setenv("S3_BUCKET", "")
	t.Setenv("MONGODB_URI", "")
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"--help"}, 0},
		{[]string{"unknown"}, 2},
		{[]string{"backup", "unexpected"}, 2},
		{[]string{"extract-config"}, 2},
	} {
		os.Args = append([]string{"mongodb-backup-s3"}, tc.args...)
		if got := run(); got != tc.code {
			t.Fatalf("args=%v code=%d want=%d", tc.args, got, tc.code)
		}
	}
}
