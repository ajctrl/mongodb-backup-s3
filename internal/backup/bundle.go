package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type FileRecord struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	FormatVersion  int                   `json:"format_version"`
	Mode           string                `json:"mode"`
	Database       string                `json:"database,omitempty"`
	Oplog          bool                  `json:"oplog"`
	StartedAt      time.Time             `json:"started_at"`
	CompletedAt    time.Time             `json:"completed_at"`
	ServerVersion  string                `json:"server_version"`
	FCV            string                `json:"feature_compatibility_version"`
	ToolsVersion   string                `json:"tools_version"`
	ConfigIncluded bool                  `json:"config_included"`
	Files          map[string]FileRecord `json:"files"`
}

type countingHashWriter struct {
	dst  io.Writer
	hash hash.Hash
	n    int64
}

func (w *countingHashWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.hash.Write(p[:n])
	w.n += int64(n)
	return n, err
}

// ZIP64 supports unknown stream lengths: no full dump is staged on disk or RAM.
// The ZIP footer and successful EOF are published only after every component succeeds.
func (a *App) writeBundle(ctx context.Context, out io.Writer, r commandRunner, source Source, db string, snapshot Snapshot, toolsVersion string, started time.Time) error {
	z := zip.NewWriter(out)
	m := Manifest{FormatVersion: 1, Mode: a.Config.BackupMode, Database: db, Oplog: a.Config.BackupMode == "full", StartedAt: started, ServerVersion: snapshot.ServerVersion, FCV: snapshot.FCV, ToolsVersion: toolsVersion, ConfigIncluded: a.Config.ConfigDir != "", Files: map[string]FileRecord{}}
	add := func(name string, produce func(io.Writer) error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeMember(name) || !allowedMember(name) || name == "manifest.json" {
			return fmt.Errorf("unsupported backup member name %q", name)
		}
		if _, exists := m.Files[name]; exists {
			return fmt.Errorf("duplicate backup member %q", name)
		}
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if strings.HasSuffix(name, ".archive.gz") {
			h.Method = zip.Store
		}
		h.SetMode(0600)
		dst, err := z.CreateHeader(h)
		if err != nil {
			return err
		}
		w := &countingHashWriter{dst: dst, hash: sha256.New()}
		if err := produce(w); err != nil {
			return err
		}
		m.Files[name] = FileRecord{Size: w.n, SHA256: hex.EncodeToString(w.hash.Sum(nil))}
		return nil
	}
	if err := add("replica-set.json", func(w io.Writer) error { _, err := w.Write(snapshot.ReplicaSet); return err }); err != nil {
		return err
	}
	if err := captureConfig(ctx, a.Config.ConfigDir, func(name string, r io.Reader) error {
		return add(name, func(w io.Writer) error { _, err := io.Copy(w, r); return err })
	}); err != nil {
		return err
	}
	if err := add("data.archive.gz", func(w io.Writer) error { return r.run(ctx, w, "mongodump", a.dumpArgs(db, false)...) }); err != nil {
		return err
	}
	if a.Config.BackupMode == "per-database" {
		// MongoDB users can be defined on admin while granting access to app DBs.
		// Preserve the complete admin authentication catalog, restored explicitly.
		if err := add("auth.archive.gz", func(w io.Writer) error { return r.run(ctx, w, "mongodump", a.dumpArgs(db, true)...) }); err != nil {
			return err
		}
	}
	after, err := source.Inspect(ctx)
	if err != nil {
		return err
	}
	if !bytes.Equal(snapshot.ReplicaSet, after.ReplicaSet) || snapshot.FCV != after.FCV || snapshot.ServerVersion != after.ServerVersion {
		return fmt.Errorf("replica set configuration or server version changed during backup")
	}
	m.CompletedAt = a.now().UTC()
	manifest, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(manifest) > 4*1024*1024 {
		return fmt.Errorf("backup manifest exceeds 4 MiB; reduce configuration file count")
	}
	w, err := z.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Deflate})
	if err != nil {
		return err
	}
	if _, err := w.Write(manifest); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return z.Close()
}

func safeMember(name string) bool {
	// JSON replaces invalid UTF-8, which would change the manifest's member keys.
	return name != "" && utf8.ValidString(name) && !strings.ContainsAny(name, "\\:\x00") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && name != "." && name != ".." && !strings.HasPrefix(name, "../")
}
func allowedMember(name string) bool {
	return name == "manifest.json" || name == "replica-set.json" || name == "data.archive.gz" || name == "auth.archive.gz" || strings.HasPrefix(name, "config/")
}

// validateBundle reads every member to EOF (ZIP CRC + manifest SHA-256) before
// any database import, including components the selected operation won't use.
func validateBundle(ctx context.Context, z *zip.ReadCloser, c Config) (Manifest, error) {
	var m Manifest
	files := map[string]*zip.File{}
	for _, f := range z.File {
		if !safeMember(f.Name) || !allowedMember(f.Name) || f.FileInfo().IsDir() || f.Mode()&os.ModeType != 0 {
			return m, fmt.Errorf("invalid backup bundle member")
		}
		if files[f.Name] != nil {
			return m, fmt.Errorf("duplicate backup bundle member")
		}
		files[f.Name] = f
	}
	mf := files["manifest.json"]
	if mf == nil || mf.UncompressedSize64 > 4*1024*1024 {
		return m, fmt.Errorf("missing or oversized backup manifest")
	}
	r, err := mf.Open()
	if err != nil {
		return m, err
	}
	b, err := io.ReadAll(io.LimitReader(r, 4*1024*1024+1))
	err = errors.Join(err, r.Close())
	if err != nil {
		return m, err
	}
	if len(b) > 4*1024*1024 {
		return m, fmt.Errorf("oversized backup manifest")
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("invalid backup manifest")
	}
	if m.FormatVersion != 1 || m.Mode != c.BackupMode || m.Oplog != (m.Mode == "full") || m.Database != c.Database {
		return m, fmt.Errorf("backup manifest does not match selected mode/database")
	}
	if m.ServerVersion == "" || m.ToolsVersion == "" || m.FCV == "" || m.StartedAt.IsZero() || m.CompletedAt.Before(m.StartedAt) {
		return m, fmt.Errorf("incomplete backup manifest")
	}
	if files["data.archive.gz"] == nil || files["replica-set.json"] == nil || (m.Mode == "per-database" && files["auth.archive.gz"] == nil) {
		return m, fmt.Errorf("backup is missing required components")
	}
	if m.Mode == "full" && files["auth.archive.gz"] != nil {
		return m, fmt.Errorf("unexpected authentication archive in full bundle")
	}
	if len(m.Files) != len(files)-1 {
		return m, fmt.Errorf("backup manifest file inventory mismatch")
	}
	configs := 0
	for name, f := range files {
		if name == "manifest.json" {
			continue
		}
		record, ok := m.Files[name]
		if !ok || record.Size < 0 || uint64(record.Size) != f.UncompressedSize64 {
			return m, fmt.Errorf("backup file size mismatch for %q", name)
		}
		if strings.HasPrefix(name, "config/") {
			configs++
		}
		r, err := f.Open()
		if err != nil {
			return m, err
		}
		h := sha256.New()
		n, err := io.Copy(h, contextReader{ctx: ctx, r: r})
		err = errors.Join(err, r.Close())
		if err != nil {
			return m, fmt.Errorf("verify backup member %q: %w", name, err)
		}
		if n != record.Size || hex.EncodeToString(h.Sum(nil)) != record.SHA256 {
			return m, fmt.Errorf("backup checksum mismatch for %q", name)
		}
	}
	if m.ConfigIncluded != (configs > 0) {
		return m, fmt.Errorf("configuration inventory mismatch")
	}
	return m, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func zipMember(z *zip.ReadCloser, name string) (*zip.File, error) {
	for _, f := range z.File {
		if f.Name == name {
			return f, nil
		}
	}
	return nil, fmt.Errorf("missing backup member %q", name)
}
func copyMember(ctx context.Context, f *zip.File, dest string) error {
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, contextReader{ctx: ctx, r: r})
	return errors.Join(err, w.Close())
}

func extractConfiguration(ctx context.Context, z *zip.ReadCloser, dest string) (err error) {
	dest, err = filepath.Abs(dest)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(dest); !os.IsNotExist(err) {
		return fmt.Errorf("configuration output directory must not already exist")
	}
	// Stage next to the destination, then rename so failed extraction is not exposed.
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".mongodb-config-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for _, f := range z.File {
		if f.Name != "manifest.json" && f.Name != "replica-set.json" && !strings.HasPrefix(f.Name, "config/") {
			continue
		}
		if !safeMember(f.Name) {
			return fmt.Errorf("invalid configuration member")
		}
		target := filepath.Join(dir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := copyMember(ctx, f, target); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(dir, dest)
}
