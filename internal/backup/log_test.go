package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestDatabaseAndKeyLogsEscapeControlCharacters(t *testing.T) {
	const database = "app\nFORGED\r\t\x1b[31m"
	for _, discovery := range []bool{false, true} {
		for _, outcome := range []string{"success", "dump failure", "upload failure", "retention failure"} {
			t.Run(fmt.Sprintf("discovery=%t/%s", discovery, outcome), func(t *testing.T) {
				a, store, source, _ := newAppTest(t, "per-database")
				var output bytes.Buffer
				a.Out, a.Err = &output, &output
				a.Config.Prefix = "prefix\nFORGED\r\t\x1b[31m"
				a.Config.Database = ""
				source.names = []string{database}
				if discovery {
					a.Config.BackupAll = "true"
				} else {
					encoded, err := json.Marshal([]string{database})
					if err != nil {
						t.Fatal(err)
					}
					a.Config.Databases, err = parseDatabaseList(string(encoded))
					if err != nil {
						t.Fatal(err)
					}
				}
				switch outcome {
				case "dump failure":
					t.Setenv("TEST_DUMP_FAILURE", "data")
				case "upload failure":
					store.uploadErr = errors.New("injected upload failure")
				case "retention failure":
					a.Config.KeepDays = "1"
					now := a.now()
					a.now = func() time.Time { return now.Add(time.Hour) }
				}
				err := a.Backup(t.Context())
				if (err == nil) != (outcome == "success") {
					t.Fatalf("backup error=%v, outcome=%s", err, outcome)
				}
				want := "Backup complete: "
				if outcome == "dump failure" || outcome == "upload failure" {
					want = "Backup failed ("
				} else if outcome == "retention failure" {
					want = "Retention failed ("
				}
				if !strings.Contains(output.String(), want+fmt.Sprintf("%q", database)) {
					t.Fatalf("missing escaped database label: %q", output.String())
				}
				if !strings.Contains(output.String(), fmt.Sprintf("key: %q", store.lastUploadedKey)) {
					t.Fatalf("missing escaped S3 key: %q", output.String())
				}
				if outcome == "success" {
					a.Config.Database, a.Config.Databases, a.Config.BackupAll = database, nil, "false"
					if err := a.Restore(t.Context(), nil); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(output.String(), fmt.Sprintf("Downloading backup: %q", store.lastUploadedKey)) {
						t.Fatalf("missing escaped download key: %q", output.String())
					}
				}
				if strings.Contains(output.String(), "\nFORGED") || strings.IndexFunc(output.String(), func(r rune) bool {
					return unicode.IsControl(r) && r != '\n'
				}) >= 0 {
					t.Fatalf("control characters escaped the log record: %q", output.String())
				}
			})
		}
	}
}
