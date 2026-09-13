package backup

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *App) validateRestore(args []string) (string, string, error) {
	c := a.Config
	if c.BackupMode == "full" {
		if c.Database != "" || c.Databases != "" || c.Exclude != "" || c.BackupAll == "true" {
			return "", "", fmt.Errorf("full restore requires clearing database selectors")
		}
	} else {
		if err := validateDatabases([]string{c.Database}); err != nil {
			return "", "", fmt.Errorf("set MONGODB_DATABASE to the database to restore: %w", err)
		}
		if c.Databases != "" || c.Exclude != "" || c.BackupAll == "true" {
			return "", "", fmt.Errorf("per-database restore accepts only MONGODB_DATABASE")
		}
	}
	switch len(args) {
	case 0:
		return "", "", nil
	case 1:
		if c.FilenameMode != "timestamp" || !timestampPattern.MatchString(args[0]) {
			break
		}
		if _, err := time.Parse(timestampLayout, args[0]); err != nil {
			break
		}
		return args[0], "", nil
	case 2:
		if c.FilenameMode == "fixed" && args[0] == "--version-id" && args[1] != "" {
			return "", args[1], nil
		}
	}
	return "", "", fmt.Errorf("use [timestamp] in timestamp mode or [--version-id ID] in fixed mode")
}
func (a *App) Restore(ctx context.Context, args []string) error { return a.restore(ctx, args, false) }
func (a *App) RestoreAuth(ctx context.Context, args []string) error {
	if a.Config.BackupMode != "per-database" {
		return fmt.Errorf("restore-auth requires per-database mode; full restore already includes authentication")
	}
	return a.restore(ctx, args, true)
}
func (a *App) restore(ctx context.Context, args []string, auth bool) error {
	if err := a.Config.requireConnection(); err != nil {
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
		return a.withBundle(ctx, args, r, func(z *zip.ReadCloser, m Manifest, _ string) error {
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
					return err
				}
			}
			if auth {
				cmd = append(cmd, "--nsInclude=admin.*")
			} else if m.Oplog {
				cmd = append(cmd, "--oplogReplay")
			} else {
				cmd = append(cmd, "--nsInclude="+a.Config.Database+".*")
			}
			fmt.Fprintf(a.Out, "Restoring backup created with %s (MongoDB %s, FCV %s)...\n", m.ToolsVersion, m.ServerVersion, m.FCV)
			if err := a.restoreArchive(ctx, r, f, cmd...); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "Restore complete.")
			return nil
		})
	})
}

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
	return a.withRunner(nil, func(r commandRunner) error {
		return a.withBundle(ctx, args, r, func(z *zip.ReadCloser, _ Manifest, _ string) error {
			if err := extractConfiguration(ctx, z, dest); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "Configuration extracted; review paths, hosts, permissions and certificates before applying.")
			return nil
		})
	})
}

func (a *App) withBundle(ctx context.Context, args []string, r commandRunner, action func(*zip.ReadCloser, Manifest, string) error) error {
	timestamp, version, err := a.validateRestore(args)
	if err != nil {
		return err
	}
	key := ""
	c := a.Config
	if c.FilenameMode == "fixed" {
		key = c.fixedKey(c.Database)
	} else if timestamp != "" {
		key = c.timestampPrefix(c.Database) + timestamp + c.fileType()
	} else {
		objects, err := a.timestampBackups(ctx, c.Database)
		if err != nil {
			return err
		}
		for _, obj := range objects {
			if strings.HasSuffix(obj.Key, c.fileType()) && obj.Key > key {
				key = obj.Key
			}
		}
		if key == "" {
			return fmt.Errorf("no matching backup found")
		}
	}
	return withWorkspace(func(dir string) error {
		file := filepath.Join(dir, "backup"+c.fileType())
		fmt.Fprintln(a.Out, "Downloading backup...")
		if err := a.Store.Download(ctx, key, version, file); err != nil {
			return err
		}
		if c.Passphrase != "" {
			plain := filepath.Join(dir, "backup.zip")
			w, err := os.OpenFile(plain, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			err = r.run(ctx, w, "gpg", "--decrypt", "--batch", "--pinentry-mode", "loopback", file)
			err = errors.Join(err, w.Close())
			if err != nil {
				return err
			}
			file = plain
		}
		z, err := zip.OpenReader(file)
		if err != nil {
			return fmt.Errorf("open backup bundle: %w", err)
		}
		defer z.Close()
		m, err := validateBundle(ctx, z, c)
		if err != nil {
			return err
		}
		return action(z, m, dir)
	})
}
