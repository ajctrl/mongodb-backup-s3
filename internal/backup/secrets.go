package backup

import (
	"bytes"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

var uriInLog = regexp.MustCompile(`mongodb(?:\+srv)?://[^\s"']+`)

type secretWriter struct {
	mu       sync.Mutex
	dst      io.Writer
	pending  []byte
	secrets  []string
	dropping bool
}

func newSecretWriter(dst io.Writer, c Config) *secretWriter {
	secrets := []string{c.URI, c.Passphrase}
	if u, err := mongoURIOptions(c.URI); err == nil && u.User != nil {
		if p, ok := u.User.Password(); ok && p != "" {
			secrets = append(secrets, p, url.QueryEscape(p), url.PathEscape(p))
		}
	}
	return &secretWriter{dst: dst, secrets: secrets}
}
func (w *secretWriter) emit(line []byte) {
	s := string(line)
	for _, secret := range w.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	s = uriInLog.ReplaceAllString(s, "[REDACTED URI]")
	io.WriteString(w.dst, s)
}
func (w *secretWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		end := len(p)
		if idx >= 0 {
			end = idx + 1
		}
		part := p[:end]
		p = p[end:]
		if !w.dropping {
			if len(w.pending)+len(part) > 65536 {
				w.pending = nil
				w.dropping = true
			} else {
				w.pending = append(w.pending, part...)
			}
		}
		if idx >= 0 {
			if w.dropping {
				io.WriteString(w.dst, "[oversized tool log line omitted]\n")
			} else {
				w.emit(w.pending)
			}
			w.pending = nil
			w.dropping = false
		}
	}
	return n, nil
}
func (w *secretWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.dropping {
		w.emit(w.pending)
	}
	w.pending = nil
	w.dropping = false
}
