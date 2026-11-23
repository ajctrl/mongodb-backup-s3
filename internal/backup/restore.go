package backup

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ajctrl/mongodb-backup-s3/internal/storage"
)

func (a *App) validateRestore(args []string) (string, string, error) {
	c := a.Config
	if c.BackupMode == "full" {
		if c.Database != "" || len(c.Databases) > 0 || len(c.Exclude) > 0 || c.BackupAll == "true" {
			return "", "", fmt.Errorf("full restore requires clearing database selectors")
		}
	} else {
		if err := validateDatabases([]string{c.Database}); err != nil {
			return "", "", fmt.Errorf("set MONGODB_DATABASE to the database to restore: %w", err)
		}
		if len(c.Databases) > 0 || len(c.Exclude) > 0 || c.BackupAll == "true" {
			return "", "", fmt.Errorf("per-database restore accepts only MONGODB_DATABASE")
		}
	}
	switch len(args) {
	case 0:
		return "", "", nil
	case 1:
		if c.FilenameMode != "timestamp" || !validGeneration(args[0]) {
			break
		}
		return args[0], "", nil
	case 2:
		if c.FilenameMode == "fixed" && args[0] == "--version-id" && args[1] != "" {
			if args[1] == "null" {
				return "", "", fmt.Errorf("S3 null versions are mutable; specify an immutable version ID or a timestamp generation")
			}
			return "", args[1], nil
		}
	}
	return "", "", fmt.Errorf("use [generation] in timestamp mode or [--version-id ID] in fixed mode")
}
func (a *App) Restore(ctx context.Context, args []string) error { return a.restore(ctx, args, false) }
func (a *App) RestoreAuth(ctx context.Context, args []string) error {
	if a.Config.BackupMode != "per-database" {
		return fmt.Errorf("restore-auth requires per-database mode; full restore includes authentication and partial backups contain application data only")
	}
	return a.restore(ctx, args, true)
}
func (a *App) restore(ctx context.Context, args []string, auth bool) error {
	if err := a.Config.requireRestoreConnection(); err != nil {
		return err
	}
	if _, _, err := a.validateRestore(args); err != nil {
		return err
	}
	lock, err := acquireLock(a.lockPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	return a.withRunner(lock, func(r commandRunner) error {
		return a.withBundle(ctx, args, r, func(z *zip.Reader, m Manifest) error {
			if len(m.ExcludedConfigCollections) > 0 {
				fmt.Fprintf(a.Err, "Warning: this backup excludes config database collections %q.\n", m.ExcludedConfigCollections)
			}
			if m.Oplog || a.Config.RestoreDrop {
				if err := a.checkRestoreVersions(ctx, m); err != nil {
					return err
				}
			}
			name := "data.archive.gz"
			if auth {
				name = "auth.archive.gz"
				fmt.Fprintln(a.Err, "Restoring the admin database and all saved users/roles; this affects authentication for all databases.")
			}
			f, err := zipMember(z, name)
			if err != nil {
				return err
			}
			// The complete bundle is already verified. Feed its compressed member
			// directly to the tool to avoid a third full-size temporary copy.
			// Tools may raise numParallelCollections to the archive's recorded
			// concurrency to consume its multiplexed streams; it is not a hard cap.
			cmd := append(a.connectionArgs(), "--archive", "--gzip", "--stopOnError", fmt.Sprintf("--numParallelCollections=%d", a.Config.ParallelCollections))
			if a.Config.RestoreDrop {
				cmd = append(cmd, "--drop")
			}
			if (auth || m.Oplog) && a.Config.RestoreDrop {
				// Replacing a destination user's identity can invalidate the tool's
				// authenticated connections even when its password is unchanged.
				// Restore users/roles first, then authenticate afresh before the
				// remaining restore reaches oplog replay or admin index creation.
				fmt.Fprintln(a.Out, "Restoring users and roles before reconnecting...")
				if err := a.restoreArchive(ctx, r, f, append(cmd,
					"--nsInclude=admin.system.users", "--nsInclude=admin.system.roles",
					"--nsInclude=admin.system.version")...); err != nil {
					return fmt.Errorf("restore users and roles: %w", err)
				}
			}
			if auth {
				cmd = append(cmd, "--nsInclude=admin.*")
			} else if m.Oplog {
				cmd = append(cmd, "--oplogReplay")
			} else {
				// mongorestore treats '*' as a namespace wildcard. Database
				// names on Linux may contain literal stars; backslash escapes
				// them in the tool's pattern grammar (not shell quoting).
				cmd = append(cmd, "--nsInclude="+strings.ReplaceAll(a.Config.Database, "*", `\*`)+".*")
			}
			if m.Mode == "partial" {
				fmt.Fprintf(a.Out, "Restoring partial data for %q; cluster database inventory was unverified.\n", m.Database)
			} else {
				fmt.Fprintf(a.Out, "Restoring backup created with %s (MongoDB %s, FCV %s)...\n", m.ToolsVersion, m.ServerVersion, m.FCV)
			}
			if err := a.restoreArchive(ctx, r, f, cmd...); err != nil {
				return fmt.Errorf("apply %s: %w", name, err)
			}
			fmt.Fprintln(a.Out, "Restore complete.")
			return nil
		})
	})
}

// Archives contain metadata and may contain an oplog. Permit patch updates
// within the same MongoDB release series while requiring matching FCVs.
func (a *App) checkRestoreVersions(ctx context.Context, m Manifest) error {
	dst, err := a.targetVersions(ctx, a.Config)
	if err != nil {
		return fmt.Errorf("inspect destructive restore target: %w", err)
	}
	target := releaseSeries.FindStringSubmatch(dst.ServerVersion)
	source := releaseSeries.FindStringSubmatch(m.ServerVersion)
	if (m.ServerVersion != "" && (source == nil || target == nil || source[1] != target[1])) || (m.FCV != "" && m.FCV != dst.FCV) {
		return fmt.Errorf("destructive restore version mismatch: source MongoDB %q FCV %q, target MongoDB %q FCV %q", m.ServerVersion, m.FCV, dst.ServerVersion, dst.FCV)
	}
	if m.ServerVersion == "" || m.FCV == "" {
		if m.Mode != "partial" || !a.Config.AllowUnknownSourceVersion {
			return fmt.Errorf("source versions unknown or incomplete; destructive partial restore requires MONGODB_RESTORE_ALLOW_UNKNOWN_SOURCE_VERSION=true")
		}
		fmt.Fprintln(a.Err, "Warning: proceeding with destructive partial restore with incomplete source versions")
	}
	return nil
}

var releaseSeries = regexp.MustCompile(`^([0-9]+[.][0-9]+)(?:[.]|$)`)

func (a *App) restoreArchive(ctx context.Context, r commandRunner, f *zip.File, args ...string) error {
	archive, err := f.Open()
	if err != nil {
		return err
	}
	defer archive.Close()
	return r.runInput(ctx, contextReader{ctx: ctx, r: archive}, a.Out, "mongorestore", args...)
}

func (a *App) ExtractConfig(ctx context.Context, dest string, args []string) error {
	if dest == "" {
		return fmt.Errorf("extract-config requires an output directory")
	}
	if _, _, err := a.validateRestore(args); err != nil {
		return err
	}
	dest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := checkConfigDestination(dest); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".mongodb-config-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	return a.withRunner(nil, func(r commandRunner) error {
		return a.withBundle(ctx, args, r, func(z *zip.Reader, _ Manifest) error {
			if err := extractConfiguration(ctx, z, dir, dest); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "Configuration extracted; review paths, hosts, permissions and certificates before applying.")
			return nil
		})
	})
}

func (a *App) withBundle(ctx context.Context, args []string, r commandRunner, action func(*zip.Reader, Manifest) error) error {
	generation, version, err := a.validateRestore(args)
	if err != nil {
		return err
	}
	var keys []string
	c := a.Config
	if version != "" {
		keys = []string{c.fixedKey(c.Database)}
	} else if generation != "" {
		keys = []string{c.timestampPrefix(c.Database) + generation + c.fileType()}
	} else {
		var newest time.Time
		err := a.Store.Walk(ctx, c.timestampPrefix(c.Database), func(obj storage.Object) error {
			fixed := c.FilenameMode == "fixed" && obj.Key == c.fixedKey(c.Database)
			if !fixed && !(c.isTimestampBackup(c.Database, obj.Key) && strings.HasSuffix(obj.Key, c.fileType())) {
				return nil
			}
			if obj.LastModified.IsZero() {
				return fmt.Errorf("missing LastModified for backup %q", obj.Key)
			}
			if obj.LastModified.After(newest) {
				keys = []string{obj.Key}
				newest = obj.LastModified
			} else if obj.LastModified.Equal(newest) {
				keys = append(keys, obj.Key)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return fmt.Errorf("no matching backup found")
		}
	}

	var selected *downloadedBundle
	defer func() {
		if selected != nil {
			selected.file.Close()
		}
	}()
	ambiguous := false
	for _, key := range keys {
		candidate, err := a.downloadBundle(ctx, key, version, r)
		if err != nil {
			return fmt.Errorf("validate backup %q: %w", key, err)
		}
		if selected == nil || candidate.manifest.CompletedAt.After(selected.manifest.CompletedAt) {
			if selected != nil {
				selected.file.Close()
			}
			selected = candidate
			ambiguous = false
		} else {
			candidate.file.Close()
			if candidate.manifest.CompletedAt.Equal(selected.manifest.CompletedAt) {
				ambiguous = true
			}
		}
	}
	if ambiguous {
		return fmt.Errorf("latest backup is ambiguous: S3 LastModified and manifest completed_at are identical; specify an explicit generation")
	}
	return action(selected.reader, selected.manifest)
}

type downloadedBundle struct {
	file     *os.File
	reader   *zip.Reader
	manifest Manifest
}

func (a *App) downloadBundle(ctx context.Context, key, version string, r commandRunner) (_ *downloadedBundle, err error) {
	fmt.Fprintf(a.Out, "Downloading backup: %q\n", key)
	file, err := anonymousFile()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			file.Close()
		}
	}()
	if err = a.Store.Download(ctx, key, version, file); err != nil {
		return nil, err
	}
	if a.Config.Passphrase != "" {
		plain, err := anonymousFile()
		if err != nil {
			return nil, err
		}
		if err := r.runInput(ctx, file, plain, "gpg", "--decrypt", "--batch", "--pinentry-mode", "loopback", "--output", "-"); err != nil {
			plain.Close()
			return nil, err
		}
		file.Close()
		file = plain
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	z, err := zip.NewReader(file, info.Size())
	if err != nil {
		return nil, fmt.Errorf("open backup bundle: %w", err)
	}
	m, err := validateBundle(ctx, z, a.Config)
	if err != nil {
		return nil, err
	}
	return &downloadedBundle{file: file, reader: z, manifest: m}, nil
}
