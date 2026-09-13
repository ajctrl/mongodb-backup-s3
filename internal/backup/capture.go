package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"strings"
)

// captureConfig adds regular files to a generation bundle. add must consume the
// entire reader before returning. Source permissions are deliberately not used
// for extraction: restored files containing credentials must remain private.
func captureConfig(ctx context.Context, dir string, add func(name string, r io.Reader) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dir == "" {
		return nil
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("cannot inspect MONGODB_CONFIG_DIR: %w", err)
	}
	if !info.IsDir() || configLink(info) {
		return errors.New("MONGODB_CONFIG_DIR must be a directory, not a symlink or reparse point")
	}
	// Root confines all file opens even if a path is replaced during traversal.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("cannot open MONGODB_CONFIG_DIR: %w", err)
	}
	defer root.Close()
	openedRoot, err := root.Stat(".")
	if err != nil || !os.SameFile(info, openedRoot) {
		return errors.New("MONGODB_CONFIG_DIR changed while opening")
	}
	type entry struct {
		name string
		info fs.FileInfo
	}
	var files []entry
	entries := make(map[string]fs.FileInfo)
	err = fs.WalkDir(root.FS(), ".", func(name string, item fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return fmt.Errorf("cannot scan config path %q: %w", name, walkErr)
		}
		info, err := root.Lstat(name)
		if err != nil {
			return fmt.Errorf("cannot inspect config path %q: %w", name, err)
		}
		if configLink(info) || strings.ContainsRune(name, '\\') {
			return fmt.Errorf("config path %q is a symlink, reparse point, or unsafe archive name", name)
		}
		entries[name] = info
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("config path %q is not a regular file", name)
		}
		files = append(files, entry{name, info})
		return nil
	})
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("MONGODB_CONFIG_DIR contains no regular files")
	}
	hashes := make(map[string][]byte, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		h := sha256.New()
		if err := captureConfigFile(ctx, root, file.name, file.info, func(name string, r io.Reader) error {
			return add(name, io.TeeReader(r, h))
		}); err != nil {
			return err
		}
		hashes[file.name] = h.Sum(nil)
	}
	// Compare the inventory as well as metadata: directory size and mtime can
	// remain unchanged when an entry is added within one filesystem clock tick.
	seen := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, _ fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return fmt.Errorf("config path %q changed during backup: %w", name, walkErr)
		}
		after, err := root.Lstat(name)
		before, exists := entries[name]
		if err != nil || !exists || !unchangedConfig(before, after) || configLink(after) {
			return fmt.Errorf("config path %q changed during backup", name)
		}
		if before.Mode().IsRegular() {
			// Same-length rewrites can also preserve mtime. Compare the current
			// contents with the bytes actually consumed by the bundle writer.
			h := sha256.New()
			if err := captureConfigFile(ctx, root, name, before, func(_ string, r io.Reader) error {
				_, err := io.Copy(h, r)
				return err
			}); err != nil {
				return err
			}
			if !bytes.Equal(hashes[name], h.Sum(nil)) {
				return fmt.Errorf("config file %q changed during backup", name)
			}
		}
		seen++
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(entries) {
		return errors.New("config directory contents changed during backup")
	}
	// OpenRoot stays attached to the original directory after a rename. Check
	// the configured path too, so a replacement cannot hide behind that handle.
	currentRoot, err := os.Lstat(dir)
	if err != nil || !os.SameFile(openedRoot, currentRoot) || configLink(currentRoot) {
		return errors.New("MONGODB_CONFIG_DIR changed during backup")
	}
	return ctx.Err()
}

func captureConfigFile(ctx context.Context, root *os.Root, name string, before fs.FileInfo, add func(string, io.Reader) error) error {
	current, err := root.Lstat(name)
	if err != nil || !unchangedConfig(before, current) || configLink(current) {
		return fmt.Errorf("config file %q changed before backup", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("cannot open config file %q: %w", name, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !unchangedConfig(before, opened) {
		return fmt.Errorf("config file %q changed while opening", name)
	}
	reader := &configReader{ctx: ctx, reader: file}
	if err := add("config/"+name, reader); err != nil {
		return fmt.Errorf("cannot save config file %q: %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader.err != nil {
		return fmt.Errorf("cannot read config file %q: %w", name, reader.err)
	}
	if reader.count != before.Size() {
		return fmt.Errorf("config file %q was not completely captured or changed during backup", name)
	}
	after, err := file.Stat()
	if err != nil || !unchangedConfig(before, after) {
		return fmt.Errorf("config file %q changed during backup", name)
	}
	current, err = root.Lstat(name)
	if err != nil || !unchangedConfig(before, current) || configLink(current) {
		return fmt.Errorf("config file %q changed during backup", name)
	}
	return nil
}

func unchangedConfig(before, after fs.FileInfo) bool {
	return after != nil && os.SameFile(before, after) && before.Mode() == after.Mode() &&
		before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func configLink(info fs.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	// FileInfo.Sys() is *syscall.Win32FileAttributeData on Windows. Reflection
	// keeps this check portable without importing Windows-only syscall types.
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if value.Kind() == reflect.Struct {
		attributes := value.FieldByName("FileAttributes")
		if attributes.IsValid() && attributes.Kind() == reflect.Uint32 {
			return attributes.Uint()&0x400 != 0 // FILE_ATTRIBUTE_REPARSE_POINT
		}
	}
	return false
}

type configReader struct {
	ctx    context.Context
	reader io.Reader
	count  int64
	err    error
}

func (r *configReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	n, err := r.reader.Read(p)
	r.count += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}
