package backup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/ajctrl/mongodb-backup-s3/internal/storage"
	"github.com/robfig/cron/v3"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type App struct {
	Config         Config
	Store          storage.Store
	Out, Err       io.Writer
	now            func() time.Time
	lockPath       string
	newSource      func(context.Context, Config) (Source, error)
	targetVersions func(context.Context, Config) (Snapshot, error)
}

func New(c Config, s storage.Store, out, stderr io.Writer) *App {
	return &App{Config: c, Store: s, Out: out, Err: stderr, now: time.Now, lockPath: "/tmp/mongodb-backup-s3.lock", newSource: NewSource, targetVersions: targetVersions}
}
func parseSchedule(spec string) (cron.Schedule, error) {
	fields := strings.Fields(spec)
	expr := fields
	if len(fields) > 0 && (strings.HasPrefix(fields[0], "TZ=") || strings.HasPrefix(fields[0], "CRON_TZ=")) {
		_, zone, _ := strings.Cut(fields[0], "=")
		if zone == "" || len(fields) < 2 {
			return nil, fmt.Errorf("timezone requires a name and a schedule")
		}
		expr = fields[1:]
	}
	if len(expr) > 0 && expr[0] == "@every" {
		if len(expr) != 2 {
			return nil, fmt.Errorf("@every requires one duration")
		}
		d, err := time.ParseDuration(expr[1])
		if err != nil || d < time.Second || d%time.Second != 0 {
			return nil, fmt.Errorf("@every requires a positive whole number of seconds")
		}
	}
	parser := cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return parser.Parse(strings.Join(fields, " "))
}
func (a *App) Run(ctx context.Context) error {
	if a.Config.Schedule == "" {
		return a.Backup(ctx)
	}
	if _, err := a.Config.validateBackup(); err != nil {
		return err
	}
	schedule, err := parseSchedule(a.Config.Schedule)
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
	redacted := newSecretWriter(a.Err, a.Config)
	defer redacted.Flush()
	var authMechanism string
	if u, err := mongoURIOptions(a.Config.URI); err == nil {
		for key, values := range u.Query() {
			if strings.EqualFold(key, "authMechanism") && len(values) == 1 {
				authMechanism = values[0]
			}
		}
	}
	r := commandRunner{authMechanism: authMechanism, passphrase: a.Config.Passphrase, stderr: redacted, lock: lock}
	return action(r.withURI(a.Config.URI))
}

func (r commandRunner) withURI(uri string) commandRunner {
	encoded, _ := json.Marshal(uri)
	r.defaults = "uri: " + string(encoded) + "\n"
	return r
}

// Effective backup and filename modes apply only to this run.
func (a App) Backup(ctx context.Context) error {
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
	if a.Config.FilenameMode == "fixed" && !a.Config.AllowUnversionedOverwrite {
		fmt.Fprintln(a.Err, "Using timestamp filenames because fixed-key overwrite permission is disabled.")
		a.Config.FilenameMode = "timestamp"
	}
	if a.Config.FilenameMode == "fixed" {
		enabled, err := a.Store.VersioningEnabled(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || !enabled {
			reason := "S3 bucket versioning is not Enabled"
			if err != nil {
				reason = fmt.Sprintf("could not verify S3 bucket versioning (%q)", err.Error())
			}
			fmt.Fprintf(a.Err, "Warning: %s. BACKUP_ALLOW_UNVERSIONED_OVERWRITE=true permits overwriting the fixed key and may replace the previous backup.\n", reason)
		}
	}
	if a.Config.BackupMode == "full" && a.Config.ReadPreference != "primary" {
		fmt.Fprintf(a.Err, "Warning: full backup uses primary instead of read preference %q.\n", a.Config.ReadPreference)
		a.Config.ReadPreference = "primary"
	}
	source, err := a.newSource(ctx, a.Config)
	if err != nil {
		return err
	}
	defer source.Close(context.Background())
	mode, databases, err := a.backupSelection(ctx, source)
	if err != nil {
		return err
	}
	a.Config.BackupMode = mode
	return a.withRunner(lock, func(r commandRunner) error {
		var version bytes.Buffer
		if err := r.run(ctx, &version, "mongodump", "--version"); err != nil {
			return err
		}
		toolsVersion := strings.TrimSpace(strings.SplitN(version.String(), "\n", 2)[0])
		if toolsVersion == "" {
			return fmt.Errorf("mongodump returned no version")
		}
		err = a.backupSelected(ctx, r, source, databases, toolsVersion, days)
		if !errors.Is(err, errFullInventoryLost) || ctx.Err() != nil {
			return err
		}
		databases, err = a.partialDatabases(ctx, source, err)
		if err != nil {
			return err
		}
		a.Config.BackupMode = "partial"
		return a.backupSelected(ctx, r, source, databases, toolsVersion, days)
	})
}

func (a *App) backupSelected(ctx context.Context, r commandRunner, source Source, databases []string, toolsVersion string, days int) error {
	var failures []error
	var partialDatabases []string
	for _, db := range databases {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := a.now().UTC()
		key, err := a.dumpSelectedDatabase(ctx, r, source, db, toolsVersion)

		if err != nil {
			fmt.Fprintf(a.Err, "Backup failed (%s): %v\n", a.label(db), err)
			failures = append(failures, err)
			continue
		}
		if a.Config.BackupMode == "partial" {
			partialDatabases = append(partialDatabases, db)
			fmt.Fprintf(a.Out, "Partial data saved for %q (key: %q). To restore, set BACKUP_MODE=partial, MONGODB_DATABASE=%q and BACKUP_FILENAME_MODE=%s.\n", db, key, db, a.Config.FilenameMode)
		} else {
			fmt.Fprintf(a.Out, "Backup complete: %s (key: %q)\n", a.label(db), key)
		}
		if a.Config.FilenameMode == "timestamp" {
			generation := strings.TrimSuffix(strings.TrimPrefix(key, a.Config.timestampPrefix(db)), a.Config.fileType())
			fmt.Fprintf(a.Out, "To restore %s using your existing restore setup, set BACKUP_FILENAME_MODE=timestamp and run mongodb-backup-s3 restore %s\n", a.label(db), generation)
		}
		if days > 0 {
			if err := a.removeOldBackups(ctx, db, key, started, a.now().UTC(), days); err != nil {
				failures = append(failures, err)
				fmt.Fprintf(a.Err, "Retention failed (%s): %v\n", a.label(db), err)
			}
		}
	}
	if a.Config.BackupMode == "partial" {
		failures = append(failures, fmt.Errorf("full backup incomplete: cluster database inventory unverified; saved partial databases: %q", partialDatabases))
	}
	if len(failures) > 0 {
		return fmt.Errorf("backup run completed with failures: %w", errors.Join(failures...))
	}
	return nil
}

func (a App) dumpSelectedDatabase(ctx context.Context, runner commandRunner, source Source, db, toolsVersion string) (string, error) {
	var election bson.ObjectID
	if a.Config.BackupMode == "per-database" && a.Config.ReadPreference != "primary" {
		uri, selectedElection, err := source.dumpMember(ctx, a.Config.URI)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			fmt.Fprintf(a.Err, "Warning: cannot verify %s reads for %q (%v); using the primary for this database.\n", a.Config.ReadPreference, db, err)
			source = source.primaryView()
			a.Config.ReadPreference = "primary"
			uri, selectedElection, err = source.dumpMember(ctx, a.Config.URI)
			if err != nil {
				return "", fmt.Errorf("verify fallback primary: %w", err)
			}
		}
		runner = runner.withURI(uri)
		election = selectedElection
	}
	snapshot, err := source.Inspect(ctx, a.Config.BackupMode)
	if err != nil {
		return "", err
	}
	if !election.IsZero() && election != snapshot.ElectionID {
		return "", errors.New("primary election changed before database dump")
	}
	if a.Config.BackupMode == "partial" && (snapshot.ServerVersion == "" || snapshot.FCV == "" || len(snapshot.ReplicaSet) == 0) {
		fmt.Fprintf(a.Err, "Warning: partial backup of %q has incomplete cluster metadata.\n", db)
	}
	return a.backupDatabase(ctx, runner, source, db, snapshot, toolsVersion)
}

func (a *App) label(db string) string {
	if a.Config.BackupMode == "full" {
		return "full replica set"
	}
	return strconv.Quote(db)
}
func (a *App) backupSelection(ctx context.Context, source Source) (string, []string, error) {
	all, err := source.Databases(ctx, false, "")
	if a.Config.BackupMode == "full" {
		if err == nil {
			return "full", []string{""}, validateFullInventory(all)
		}
		dbs, err := a.partialDatabases(ctx, source, err)
		return "partial", dbs, err
	}
	if err != nil {
		return "", nil, err
	}
	fmt.Fprintln(a.Err, "Warning: per-database backups do not provide a point-in-time snapshot while writes continue.")
	dbs, err := a.Config.selectDatabases(all)
	return a.Config.BackupMode, dbs, err
}

// mongodump excludes every name with this prefix during a full dump.
func validateFullInventory(names []string) error {
	for _, name := range names {
		if strings.HasPrefix(name, "__mdb_internal") {
			return fmt.Errorf("full backup refused: mongodump excludes database %q", name)
		}
	}
	return nil
}

var errFullInventoryLost = errors.New("cannot confirm full database inventory after dump")

func (a *App) partialDatabases(ctx context.Context, source Source, cause error) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fmt.Fprintf(a.Err, "Warning: full inventory unverified (%v). Attempting partial backups of visible application databases without oplog.\n", cause)
	if err := a.Config.requireDatabaseAuthSource(); err != nil {
		return nil, errors.Join(cause, err)
	}
	visible, err := source.Databases(ctx, true, "")
	if err != nil {
		return nil, fmt.Errorf("cannot list visible databases for partial backup: %w", errors.Join(cause, err))
	}
	c := a.Config
	c.BackupAll = "true"
	return c.selectDatabases(visible)
}

func (c Config) selectDatabases(all []string) ([]string, error) {
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
	} else if len(c.Databases) > 0 {
		names = c.Databases
	} else {
		names = []string{c.Database}
	}
	excluded := map[string]bool{}
	for _, name := range c.Exclude {
		excluded[name] = true
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
	if err := validateDatabases(selected); err != nil {
		return nil, err
	}
	// mongodump can succeed for a nonexistent database. Check every selected
	// database before publishing any bundle from this invocation.
	for _, name := range selected {
		if !exists[name] {
			return nil, fmt.Errorf("selected MongoDB database %q does not exist", name)
		}
	}
	return selected, nil
}
func (a *App) connectionArgs() []string {
	var args []string
	if a.Config.TLSCA != "" {
		args = append(args, "--ssl", "--sslCAFile="+a.Config.TLSCA)
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

func (a *App) backupDatabase(ctx context.Context, r commandRunner, source Source, db string, snapshot Snapshot, toolsVersion string) (string, error) {
	now := a.now().UTC()
	key := a.Config.backupKey(db, now)
	fmt.Fprintf(a.Out, "Creating and uploading backup: %s (key: %q)\n", a.label(db), key)
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
	err := a.Store.Upload(ctx, key, reader, a.Config.FilenameMode == "fixed")
	if err != nil {
		cancel()
		reader.CloseWithError(err)
	}
	return key, errors.Join(err, <-done)
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

// S3 may timestamp multipart objects at initiation rather than completion.
// Compare its timestamp with the entire capture/upload interval, allowing for
// timestamp precision and small clock differences, before deleting anything.
const retentionClockTolerance = 5 * time.Minute

func (a *App) removeOldBackups(ctx context.Context, db, publishedKey string, started, completed time.Time, days int) error {
	// Spool managed keys while validating all pages. Deletion begins only after
	// the complete listing, timestamps and local clock have passed validation.
	spool, err := os.CreateTemp("", "mongodb-backup-s3-retention-")
	if err != nil {
		return err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	buffer := bufio.NewWriter(spool)
	enc := gob.NewEncoder(buffer)
	var published, newest time.Time
	err = a.Store.Walk(ctx, a.Config.timestampPrefix(db), func(obj storage.Object) error {
		managed := a.Config.isTimestampBackup(db, obj.Key)
		if !managed && obj.Key != publishedKey {
			return nil
		}
		if obj.LastModified.IsZero() {
			return fmt.Errorf("missing modification time for %q", obj.Key)
		}
		if obj.Key == publishedKey {
			published = obj.LastModified
		}
		if !managed {
			return nil
		}
		if obj.LastModified.After(newest) {
			newest = obj.LastModified
		}
		return enc.Encode(obj)
	})
	if err != nil {
		return err
	}
	if err := buffer.Flush(); err != nil {
		return err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if published.IsZero() {
		return fmt.Errorf("retention refused: newly published backup %q is missing from S3 listing", publishedKey)
	}
	if completed.Before(started) || published.Before(started.Add(-retentionClockTolerance)) || published.After(completed.Add(retentionClockTolerance)) {
		return fmt.Errorf("retention refused: S3 timestamp %s is outside the backup interval %s to %s (clock tolerance %s); backup saved, no objects deleted", published.Format(time.RFC3339Nano), started.Format(time.RFC3339Nano), completed.Format(time.RFC3339Nano), retentionClockTolerance)
	}
	// Use the new object's S3 timestamp, not the host clock, as the age reference.
	cutoff := published.Add(-time.Duration(days) * 24 * time.Hour)
	batch := make([]string, 0, storage.MaxDeleteKeys)
	dec := gob.NewDecoder(bufio.NewReader(spool))
	for {
		var obj storage.Object
		err := dec.Decode(&obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Preserve the published generation and all ties for newest. This also
		// leaves at least one timestamp generation when publishing a fixed key.
		if obj.Key == publishedKey || obj.LastModified.Equal(newest) {
			continue
		}
		if obj.LastModified.Before(cutoff) {
			batch = append(batch, obj.Key)
			if len(batch) == storage.MaxDeleteKeys {
				if err := a.Store.DeleteBatch(ctx, batch); err != nil {
					return err
				}
				batch = batch[:0]
			}
		}
	}
	if len(batch) > 0 {
		return a.Store.DeleteBatch(ctx, batch)
	}
	return nil
}
