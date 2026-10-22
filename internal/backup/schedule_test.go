package backup

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type scheduleLog struct{ lines chan string }

func (l scheduleLog) Write(p []byte) (int, error) {
	select {
	case l.lines <- string(p):
	default:
	}
	return len(p), nil
}
func (l scheduleLog) wait(t *testing.T, text string) {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-l.lines:
			if strings.Contains(line, text) {
				return
			}
		case <-timer.C:
			t.Fatalf("did not observe %q", text)
		}
	}
}

type scheduledStore struct {
	*appTestStore
	mu       sync.Mutex
	attempts int
	block    bool
	canceled chan struct{}
	release  chan struct{}
}

func (s *scheduledStore) Upload(ctx context.Context, key string, body io.ReadCloser, overwrite bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.block {
		defer body.Close()
		// Read from the real bundle producer before holding an active upload.
		if _, err := io.CopyN(io.Discard, body, 1); err != nil {
			return err
		}
		<-ctx.Done()
		close(s.canceled)
		<-s.release // Model cleanup that must finish before Run returns.
		return ctx.Err()
	}
	if s.attempts == 1 {
		body.Close()
		return errors.New("injected upload failure")
	}
	return s.appTestStore.Upload(ctx, key, body, overwrite)
}

func startScheduledTest(t *testing.T, a *App) (context.CancelFunc, <-chan error, scheduleLog) {
	t.Helper()
	a.Config.Schedule = "@every 1s"
	a.now = time.Now
	log := scheduleLog{make(chan string, 128)}
	a.Out, a.Err = log, log
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { defer close(exited); done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			t.Error("scheduled runner did not stop")
		}
	})
	return cancel, done, log
}

func waitScheduledStop(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("scheduler shutdown timed out")
	}
}

func TestScheduledBackupsContinueAfterFailure(t *testing.T) {
	a, store, _, _ := newAppTest(t, "full")
	// Match production: each scheduled backup opens its own source connection.
	a.newSource = func(context.Context, Config) (Source, error) { return &appTestSource{}, nil }
	s := &scheduledStore{appTestStore: store}
	a.Store = s
	cancel, done, log := startScheduledTest(t, a)
	log.wait(t, "injected upload failure")
	log.wait(t, "Backup complete:")
	log.wait(t, "Backup complete:")
	cancel()
	waitScheduledStop(t, done)
	if s.attempts < 3 || len(store.objects) < 2 {
		t.Fatalf("failed tick stopped recurrence: attempts=%d objects=%d", s.attempts, len(store.objects))
	}
	lock, err := acquireLock(a.lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestScheduledBackupSkipsOverlapAndWaitsForCancellationCleanup(t *testing.T) {
	a, store, source, _ := newAppTest(t, "full")
	a.Config.KeepDays = "1"
	s := &scheduledStore{appTestStore: store, block: true, canceled: make(chan struct{}), release: make(chan struct{})}
	a.Store = s
	cancel, done, log := startScheduledTest(t, a)
	// Registered last so even an assertion failure releases the blocked store
	// before the runner cleanup waits for shutdown.
	t.Cleanup(func() {
		select {
		case <-s.release:
		default:
			close(s.release)
		}
	})
	log.wait(t, "Could not acquire backup lock")
	cancel()
	select {
	case <-s.canceled:
	case <-time.After(15 * time.Second):
		t.Fatal("upload not canceled")
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned before upload cleanup: %v", err)
	default:
	}
	if lock, err := acquireLock(a.lockPath); err == nil {
		lock.Close()
		t.Fatal("lock released during upload cleanup")
	}
	close(s.release)
	waitScheduledStop(t, done)
	if s.attempts != 1 || len(store.objects) != 0 || store.lists != 0 || !source.closed {
		t.Fatalf("overlap/cancellation leaked work: attempts=%d objects=%d lists=%d closed=%v", s.attempts, len(store.objects), store.lists, source.closed)
	}
	lock, err := acquireLock(a.lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestInvalidScheduleStartsNoBackup(t *testing.T) {
	for _, schedule := range []string{"invalid", "0 0 30 2 *"} {
		t.Run(schedule, func(t *testing.T) {
			a, store, source, _ := newAppTest(t, "full")
			a.Config.Schedule = schedule
			if err := a.Run(t.Context()); err == nil {
				t.Fatal("invalid schedule accepted")
			}
			if source.inspections != 0 || store.uploads != 0 {
				t.Fatal("invalid schedule started a backup")
			}
		})
	}
}
