//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSignalsDuringScheduledMultipartUpload(t *testing.T) {
	ctx, cancel := fixtureContext(t, 5*time.Minute)
	defer cancel()
	f := newFixture(t, ctx)
	source := f.connect(t, f.sourceURI)
	// More than one MongoDB batch and one S3 part, even after compression.
	documents := make([]any, 256)
	for i := range documents {
		payload := make([]byte, 128*1024)
		rand.Read(payload)
		documents[i] = bson.M{"_id": i, "payload": payload}
	}
	if _, err := source.Database("app").Collection("payload").InsertMany(ctx, documents); err != nil {
		t.Fatal(err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		if !t.Run(sig.String(), func(t *testing.T) {
			// Allow the first getMore so mongodump flushes its archive buffer,
			// then hold the next batch while S3 has uploaded actual parts.
			f.command(t, source.Database("admin"), bson.D{
				{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: bson.M{"skip": 1}},
				{Key: "data", Value: bson.M{"failCommands": bson.A{"getMore"}, "blockConnection": true, "blockTimeMS": 120000}},
			})
			defer f.command(t, source.Database("admin"), bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}})
			prefix := fmt.Sprintf("signal-%d", sig)
			key := prefix + "/per-database/app/latest.zip"
			env := map[string]string{"S3_PREFIX": prefix, "BACKUP_MODE": "per-database", "MONGODB_DATABASE": "app", "BACKUP_FILENAME_MODE": "fixed", "BACKUP_ALLOW_UNVERSIONED_OVERWRITE": "true", "SCHEDULE": "@every 1s", "S3_UPLOAD_PART_SIZE_MB": "5"}
			cmd, signal := f.backupCommand(t, env, "run")
			logPath := filepath.Join(t.TempDir(), "backup.log")
			log, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			exited := make(chan struct{})
			go func() { defer close(exited); done <- cmd.Wait() }()
			defer func() {
				select {
				case <-exited:
				default:
					_ = signal(syscall.SIGTERM)
					select {
					case <-exited:
					case <-time.After(20 * time.Second):
						_ = signal(syscall.SIGKILL)
						_ = cmd.Process.Kill()
						<-exited
					}
				}
				if t.Failed() {
					data, _ := os.ReadFile(logPath)
					t.Logf("backup log:\n%s", data)
				}
			}()
			waitFor(t, ctx, "multipart upload in progress", func(ctx context.Context) error {
				select {
				case <-exited:
					t.Fatal("scheduled backup exited before signal")
				default:
				}
				// Silo's nonempty multipart prefix lookup requires the exact key.
				pending, err := f.s3.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(f.bucket), Prefix: aws.String(key)})
				if err != nil {
					return err
				}
				if len(pending.Uploads) != 1 {
					return fmt.Errorf("pending uploads=%d", len(pending.Uploads))
				}
				parts, err := f.s3.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(f.bucket), Key: pending.Uploads[0].Key, UploadId: pending.Uploads[0].UploadId})
				if err != nil {
					return err
				}
				if len(parts.Parts) == 0 {
					return fmt.Errorf("first part has not arrived")
				}
				return nil
			})
			if err := signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 128+int(sig) {
					t.Fatalf("signal %v: %v", sig, err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("signal shutdown did not finish")
			}
			if keys := f.keys(t, prefix+"/"); len(keys) != 0 {
				t.Fatalf("incomplete backup published: %v", keys)
			}
			pending, err := f.s3.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(f.bucket), Prefix: aws.String(key)})
			if err != nil || len(pending.Uploads) != 0 {
				t.Fatalf("multipart upload not aborted: %+v %v", pending, err)
			}
			if f.native {
				lock, err := os.OpenFile("/tmp/mongodb-backup-s3.lock", os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatalf("parent or dump child retained backup lock: %v", err)
				}
			}
		}) {
			return
		}
	}
}
