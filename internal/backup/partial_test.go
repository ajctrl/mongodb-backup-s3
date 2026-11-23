package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type inventorySource struct {
	*appTestSource
	list func(authorizedOnly bool) ([]string, error)
}

func (s inventorySource) Databases(_ context.Context, authorizedOnly bool, _ string) ([]string, error) {
	return s.list(authorizedOnly)
}

func TestUnverifiedInventoryFallsBackOnlyInFullMode(t *testing.T) {
	for _, mode := range []string{"full", "per-database"} {
		t.Run(mode, func(t *testing.T) {
			a, store, source, _ := newAppTest(t, mode)
			a.Config.FilenameMode = "fixed"
			store.versions = false // Both fallbacks must work together.
			a.Config.KeepDays = "1"
			old := a.Config.timestampPrefix("") + "2020-01-01T00:00:00-00000000000000000000000000000001.zip"
			store.objects[old], store.modified[old] = []byte("keep"), a.now().Add(-72*time.Hour)
			visible := []string{"admin", "app", "config", "local", "missing"}
			src := inventorySource{source, func(authorizedOnly bool) ([]string, error) {
				if !authorizedOnly {
					return nil, errUnauthorized
				}
				names := visible
				visible = []string{"app"} // One DB vanishes before its archive is committed.
				return names, nil
			}}
			a.newSource = func(context.Context, Config) (Source, error) { return src, nil }
			err := a.Backup(t.Context())
			if err == nil || string(store.objects[old]) != "keep" || len(store.deleted) != 0 {
				t.Fatalf("original backup not protected: err=%v deleted=%v", err, store.deleted)
			}
			if mode == "per-database" {
				if store.uploads != 0 || !errors.Is(err, errUnauthorized) {
					t.Fatalf("per-database attempted fallback: uploads=%d err=%v", store.uploads, err)
				}
				return
			}
			if len(store.objects) != 2 || !strings.Contains(err.Error(), `saved partial databases: ["app"]`) || store.allowOverwrite {
				t.Fatalf("unexpected partial backup result: objects=%d err=%v overwrite=%t", len(store.objects), err, store.allowOverwrite)
			}
			recovery := *a
			recovery.Config.BackupMode, recovery.Config.Database, recovery.Config.FilenameMode = "partial", "app", "timestamp"
			if err := recovery.Restore(t.Context(), nil); err != nil || !strings.HasPrefix(store.downloadedKey, "unit/partial/app/") {
				t.Fatalf("latest partial recovery: key=%q err=%v", store.downloadedKey, err)
			}
			// Even under a full key, the partial manifest must prevent a full restore.
			store.objects[a.Config.fixedKey("")] = store.objects[store.downloadedKey]
			store.modified[a.Config.fixedKey("")] = a.now()
			if err := a.Restore(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "manifest does not match") {
				t.Fatalf("full restore accepted partial content: %v", err)
			}
			if err := recovery.RestoreAuth(t.Context(), nil); err == nil {
				t.Fatal("partial mode accepted restore-auth")
			}
			if err := recovery.Backup(t.Context()); err == nil {
				t.Fatal("partial mode accepted an explicit backup")
			}
		})
	}
}

func TestFullInventoryLostBeforePublication(t *testing.T) {
	for _, visible := range [][]string{{"app"}, nil} {
		a, store, source, _ := newAppTest(t, "full")
		a.Config.FilenameMode = "fixed"
		a.Config.AllowUnversionedOverwrite = true
		fullKey := a.Config.fixedKey("")
		store.objects[fullKey] = []byte("previous full backup")
		calls := 0
		src := inventorySource{source, func(authorizedOnly bool) ([]string, error) {
			if authorizedOnly {
				if visible == nil {
					return nil, errors.New("visible inventory unavailable")
				}
				return visible, nil
			}
			calls++
			if calls > 1 {
				return nil, errUnauthorized
			}
			return source.names, nil
		}}
		a.newSource = func(context.Context, Config) (Source, error) { return src, nil }
		err := a.Backup(t.Context())
		if err == nil || calls != 2 || string(store.objects[fullKey]) != "previous full backup" || len(store.objects) != 1+len(visible) {
			t.Fatalf("late inventory failure: calls=%d objects=%d err=%v", calls, len(store.objects), err)
		}
		if visible != nil && len(store.objects["unit/partial/app/latest.zip"]) == 0 {
			t.Fatal("partial recovery point missing")
		}
	}
}

func TestPerDatabaseMissingBeforeOrAfterDump(t *testing.T) {
	for _, disappearAt := range []int{2, 3} {
		a, store, source, _ := newAppTest(t, "per-database")
		a.Config.FilenameMode, a.Config.KeepDays = "fixed", "1"
		old := a.Config.fixedKey("app")
		store.objects[old] = []byte("previous generation")
		calls := 0
		src := inventorySource{source, func(_ bool) ([]string, error) {
			calls++
			if calls >= disappearAt {
				return nil, nil
			}
			return []string{"app"}, nil
		}}
		a.newSource = func(context.Context, Config) (Source, error) { return src, nil }
		err := a.Backup(t.Context())
		if err == nil || !strings.Contains(err.Error(), "no longer visible") || string(store.objects[old]) != "previous generation" || store.lists != 0 {
			t.Fatalf("disappeared at check %d: err=%v retention listings=%d", disappearAt, err, store.lists)
		}
	}
}

func TestFullRefusesToolExcludedDatabases(t *testing.T) {
	for _, appearsAt := range []int{1, 2} {
		a, store, source, _ := newAppTest(t, "full")
		a.Config.FilenameMode = "fixed"
		key := a.Config.fixedKey("")
		store.objects[key] = []byte("saved")
		calls := 0
		src := inventorySource{source, func(bool) ([]string, error) {
			calls++
			if calls >= appearsAt {
				return []string{"app", "__mdb_internal_reports"}, nil
			}
			return []string{"app"}, nil
		}}
		a.newSource = func(context.Context, Config) (Source, error) { return src, nil }
		err := a.Backup(t.Context())
		if err == nil || !strings.Contains(err.Error(), "__mdb_internal_reports") || string(store.objects[key]) != "saved" || store.lists != 0 {
			t.Fatalf("inventory check %d: err=%v", appearsAt, err)
		}
	}
}
func TestScopedDumpsRequireExplicitAuthSource(t *testing.T) {
	for _, mode := range []string{"per-database", "full"} {
		a, store, source, _ := newAppTest(t, mode)
		a.Config.URI = "mongodb://backup:secret@host/"
		src := inventorySource{source, func(bool) ([]string, error) { return nil, errUnauthorized }}
		a.newSource = func(context.Context, Config) (Source, error) { return src, nil }
		if err := a.Backup(t.Context()); err == nil || !strings.Contains(err.Error(), "authSource") || store.uploads != 0 {
			t.Fatalf("%s: %v uploads=%d", mode, err, store.uploads)
		}
	}
}

type electionSource struct{ *appTestSource }

func (s electionSource) Inspect(ctx context.Context, mode string) (Snapshot, error) {
	snapshot, err := s.appTestSource.Inspect(ctx, mode)
	snapshot.ElectionID[0] = byte(s.inspections)
	return snapshot, err
}

func TestElectionDuringFullBackupKeepsFixedGeneration(t *testing.T) {
	a, store, source, _ := newAppTest(t, "full")
	a.newSource = func(context.Context, Config) (Source, error) { return electionSource{source}, nil }
	a.Config.FilenameMode = "fixed"
	key := a.Config.fixedKey("")
	store.objects[key] = []byte("previous")
	err := a.Backup(t.Context())
	if err == nil || !strings.Contains(err.Error(), "primary election changed") || string(store.objects[key]) != "previous" {
		t.Fatalf("election during full backup: %v", err)
	}
}
