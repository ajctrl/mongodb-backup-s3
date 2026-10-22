//go:build integration

package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Exercise Docker's actual --env-file parser with the README examples: shell
// quoting in a dotenv block must not silently become part of a JSON value.
func TestDockerEnvFileExamples(t *testing.T) {
	if os.Getenv("MONGODB_NATIVE_INTEGRATION") == "1" {
		t.Skip("requires Docker's --env-file parser")
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	var examples []string
	for _, block := range strings.Split(string(readme), "```dotenv\n")[1:] {
		block, _, ok := strings.Cut(block, "```")
		if ok && strings.HasPrefix(block, "BACKUP_MODE=per-database\n") {
			examples = append(examples, block)
		}
	}
	if len(examples) != 2 {
		t.Fatalf("expected selection and exclusion examples, got %d", len(examples))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	f := newFixture(t, ctx)
	source := f.connect(t, f.sourceURI)
	for _, name := range []string{"app", "analytics", "scratch", "temp"} {
		if _, err := source.Database(name).Collection("probe").InsertOne(ctx, bson.M{"_id": 1}); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range []string{"selection", "exclusion"} {
		t.Run(name, func(t *testing.T) {
			var lines []string
			for key, value := range f.baseEnv {
				if strings.Contains("\n"+examples[i], "\n"+key+"=") || key == "MONGODB_CONFIG_DIR" || key == "S3_PREFIX" || key == "BACKUP_FILENAME_MODE" {
					continue
				}
				lines = append(lines, key+"="+value)
			}
			sort.Strings(lines)
			prefix := "envfile-" + name
			contents := strings.Join(lines, "\n") + "\n" + examples[i] +
				"MONGODB_CONFIG_DIR=\nS3_PREFIX=" + prefix + "\nBACKUP_FILENAME_MODE=fixed\n"
			file := filepath.Join(t.TempDir(), "backup.env")
			if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			docker(t, ctx, "run", "--rm", "--network", "container:"+f.anchor,
				"--env-file", file, f.image, "mongodb-backup-s3", "backup")
			want := prefix + "/per-database/analytics/latest.zip\n" + prefix + "/per-database/app/latest.zip"
			if got := strings.Join(f.keys(t, prefix+"/"), "\n"); got != want {
				t.Fatalf("documented env file selected unexpected databases: %s", got)
			}
		})
	}
}
