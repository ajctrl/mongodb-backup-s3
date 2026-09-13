package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/ajctrl/mongodb-backup-s3/internal/storage"
	"github.com/robfig/cron/v3"
)

type App struct {
	Config    Config
	Store     storage.Store
	Out, Err  io.Writer
	now       func() time.Time
	lockPath  string
	newSource func(context.Context, Config) (Source, error)
}

func New(c Config, s storage.Store, out, stderr io.Writer) *App {
	return &App{Config: c, Store: s, Out: out, Err: stderr, now: time.Now, lockPath: "/tmp/mongodb-backup-s3.lock", newSource: NewSource}
}
func scheduleParser() cron.Parser {
	return cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
}
func (a *App) Run(ctx context.Context) error {
	if a.Config.Schedule == "" {
		return a.Backup(ctx)
	}
	if _, err := a.Config.validateBackup(); err != nil {
		return err
	}
	schedule, err := scheduleParser().Parse(a.Config.Schedule)
	if err != nil {
		return fmt.Errorf("invalid SCHEDULE: %w", err)
	}
	if schedule.Next(a.now()).IsZero() {
		return fmt.Errorf("SCHEDULE has no future execution time")
	}
	scheduler := cron.New()
	scheduler.Schedule(schedule, cron.FuncJob(func() {
		if err := a.Backup(ctx); err != nil {
			fmt.Fprintf(a.Err, "Scheduled backup failed: %v\n", err)
		}
	}))
	fmt.Fprintf(a.Out, "Scheduling backups: %s\n", a.Config.Schedule)
	scheduler.Start()
	<-ctx.Done()
	<-scheduler.Stop().Done()
	return ctx.Err()
}

func (a *App) withRunner(lock *os.File, action func(commandRunner) error) (err error) {
	dir, err := os.MkdirTemp("/tmp", "mongodb-backup-s3-auth-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "tools.yml")
	// JSON-quoted strings are YAML scalars, including passwords with quotes or #.
	uri, _ := json.Marshal(a.Config.URI)
	if err := os.WriteFile(path, append([]byte("uri: "), append(uri, '\n')...), 0600); err != nil {
		return err
	}
	redacted := newSecretWriter(a.Err, a.Config)
	defer redacted.Flush()
	r := commandRunner{defaultsFile: path, stderr: redacted, lock: lock}
	if a.Config.Passphrase != "" {
		r.passphraseFile = filepath.Join(dir, "passphrase")
		if err := os.WriteFile(r.passphraseFile, []byte(a.Config.Passphrase), 0600); err != nil {
			return err
		}
	}
	return action(r)
}
func withWorkspace(action func(string) error) (err error) {
	dir, err := os.MkdirTemp("", "mongodb-backup-s3-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	return action(dir)
}

func (a *App) Backup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	days, err := a.Config.validateBackup()
	if err != nil {
		return err
	}
	lock, err := acquireLock(a.lockPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	if a.Config.FilenameMode == "fixed" {
		enabled, err := a.Store.VersioningEnabled(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(a.Err, "Warning: Could not verify S3 bucket versioning: %v. Continuing; without versioning fixed backups overwrite previous backups.\n", err)
		} else if !enabled {
			return fmt.Errorf("fixed filenames require S3 bucket versioning to be Enabled")
		}
	}
	source, err := a.newSource(ctx, a.Config)
	if err != nil {
		return err
	}
	defer source.Close(context.Background())
	databases, err := a.databases(ctx, source)
	if err != nil {
		return err
	}
	cutoff := a.now().UTC().Truncate(time.Second).Add(-time.Duration(days) * 24 * time.Hour)
	return a.withRunner(lock, func(r commandRunner) error {
		var version bytes.Buffer
		if err := r.run(ctx, &version, "mongodump", "--version"); err != nil {
			return err
		}
		toolsVersion := strings.TrimSpace(strings.SplitN(version.String(), "\n", 2)[0])
		if toolsVersion == "" {
			return fmt.Errorf("mongodump returned no version")
		}
		var failures []error
		for _, db := range databases {
			if err := ctx.Err(); err != nil {
				return err
			}
			snapshot, err := source.Inspect(ctx)
			if err == nil {
				err = a.backupDatabase(ctx, r, source, db, snapshot, toolsVersion)
			}
			if err != nil {
				fmt.Fprintf(a.Err, "Backup failed (%s): %v\n", a.label(db), err)
				failures = append(failures, err)
				continue
			}
			fmt.Fprintf(a.Out, "Backup complete: %s\n", a.label(db))
			if days > 0 {
				if err := a.removeOldBackups(ctx, db, cutoff); err != nil {
					failures = append(failures, err)
					fmt.Fprintf(a.Err, "Retention failed (%s): %v\n", a.label(db), err)
				}
			}
		}
		if len(failures) > 0 {
			return fmt.Errorf("backup run completed with failures: %w", errors.Join(failures...))
		}
		return nil
	})
}

func (a *App) label(db string) string {
	if a.Config.BackupMode == "full" {
		return "full replica set"
	}
	return db
}
func (a *App) databases(ctx context.Context, source Source) ([]string, error) {
	c := a.Config
	if c.BackupMode == "full" {
		return []string{""}, nil
	}
	fmt.Fprintln(a.Err, "Warning: per-database backups do not provide a point-in-time snapshot while writes continue.")
	all, err := source.Databases(ctx)
	if err != nil {
		return nil, err
	}
	exists := make(map[string]bool, len(all))
	for _, name := range all {
		exists[name] = true
	}
	var names []string
	if c.BackupAll == "true" {
		for _, name := range all {
			if !systemDatabase(name) {
				names = append(names, name)
			}
		}
	} else if c.Databases != "" {
		names = parseDatabaseList(c.Databases)
	} else {
		names = []string{c.Database}
	}
	if err := validateDatabases(names); err != nil {
		return nil, err
	}
	// mongodump can succeed for a nonexistent database. Check every explicit
	// selection before publishing any bundle from this invocation.
	for _, name := range names {
		if !exists[name] {
			return nil, fmt.Errorf("selected MongoDB database %q does not exist", name)
		}
	}
	excluded := map[string]bool{}
	if c.Exclude != "" {
		for _, name := range parseDatabaseList(c.Exclude) {
			excluded[name] = true
		}
	}
	seen := map[string]bool{}
	var selected []string
	for _, name := range names {
		if !excluded[name] && !seen[name] {
			selected = append(selected, name)
			seen[name] = true
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no databases selected for backup")
	}
	return selected, nil
}
func (a *App) connectionArgs() []string {
	var args []string
	if a.Config.TLSCA != "" {
		args = append(args, "--sslCAFile="+a.Config.TLSCA)
	}
	return args
}
func (a *App) dumpArgs(db string, auth bool) []string {
	args := append(a.connectionArgs(), "--archive", "--gzip", "--readPreference="+a.Config.ReadPreference, fmt.Sprintf("--numParallelCollections=%d", a.Config.ParallelCollections))
	if auth {
		return append(args, "--db=admin")
	}
	if a.Config.BackupMode == "full" {
		return append(args, "--oplog")
	}
	return append(args, "--db="+db)
}

func (a *App) backupDatabase(ctx context.Context, r commandRunner, source Source, db string, snapshot Snapshot, toolsVersion string) error {
	now := a.now().UTC()
	key := a.Config.backupKey(db, now)
	fmt.Fprintf(a.Out, "Creating and uploading backup: %s\n", a.label(db))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	stop := context.AfterFunc(ctx, func() { reader.CloseWithError(ctx.Err()) })
	defer stop()
	done := make(chan error, 1)
	go func() {
		produce := func(ctx context.Context, dst io.Writer) error {
			return a.writeBundle(ctx, dst, r, source, db, snapshot, toolsVersion, now)
		}
		err := produceEncrypted(ctx, writer, r, a.Config.Passphrase != "", produce)
		writer.CloseWithError(err)
		done <- err
	}()
	err := a.Store.Upload(ctx, key, reader)
	if err != nil {
		cancel()
		reader.CloseWithError(err)
	}
	return errors.Join(err, <-done)
}

// A producer failure is propagated as a read error, never as successful EOF.
func produceEncrypted(ctx context.Context, out io.Writer, r commandRunner, encrypted bool, produce func(context.Context, io.Writer) error) error {
	if !encrypted {
		return produce(ctx, out)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Pass a file descriptor directly to GPG. An io.Pipe would make os/exec
	// wait for a stdin copier blocked on the producer even after GPG exits.
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	defer writer.Close()
	// Only close the producer's end on cancellation. The reader must remain
	// open until runInput returns so it cannot race with exec reading its FD.
	stop := context.AfterFunc(ctx, func() { writer.Close() })
	defer stop()
	done := make(chan error, 1)
	go func() {
		err := produce(ctx, writer)
		writer.Close()
		done <- err
	}()
	err = r.runInput(ctx, reader, out, "gpg", "--symmetric", "--batch", "--pinentry-mode", "loopback", "--output", "-")
	// GPG no longer consumes input, even if it exited successfully. Stop both
	// a dump waiting for server data and a producer blocked writing the pipe.
	cancel()
	reader.Close()
	writer.Close()
	// OS pipes cannot carry producer errors. Join the result before the outer
	// upload pipe can signal successful EOF and publish the backup.
	return errors.Join(err, <-done)
}

func (a *App) timestampBackups(ctx context.Context, db string) ([]storage.Object, error) {
	objects, err := a.Store.List(ctx, a.Config.timestampPrefix(db))
	if err != nil {
		return nil, err
	}
	var backups []storage.Object
	for _, obj := range objects {
		if a.Config.isTimestampBackup(db, obj.Key) {
			backups = append(backups, obj)
		}
	}
	return backups, nil
}
func (a *App) removeOldBackups(ctx context.Context, db string, cutoff time.Time) error {
	objects, err := a.timestampBackups(ctx, db)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if obj.LastModified.IsZero() {
			return fmt.Errorf("missing modification time for %q", obj.Key)
		}
	}
	for _, obj := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if obj.LastModified.Before(cutoff) {
			if err := a.Store.Delete(ctx, obj.Key); err != nil {
				return err
			}
		}
	}
	return nil
}
