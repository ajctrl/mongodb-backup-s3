package backup

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRetentionClockSkewPreservesAllBackups(t *testing.T) {
	for _, skew := range []time.Duration{-48 * time.Hour, 48 * time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			a, store, _, _ := newAppTest(t, "full")
			serverTime := a.now()
			a.now = func() time.Time { return serverTime.Add(skew) }
			a.Config.KeepDays = "1"
			old := a.Config.backupKey("", serverTime.Add(-72*time.Hour))
			store.objects[old] = []byte("previous good backup")
			store.modified[old] = serverTime.Add(-72 * time.Hour)
			err := a.Backup(t.Context())
			if err == nil || !strings.Contains(err.Error(), "clock tolerance") {
				t.Fatalf("clock skew did not report retention failure: %v", err)
			}
			if len(store.deleted) != 0 || len(store.objects) != 2 || len(store.objects[store.lastUploadedKey]) == 0 {
				t.Fatalf("clock skew lost a backup: deleted=%v objects=%d", store.deleted, len(store.objects))
			}
		})
	}
}

func TestRetentionUsesS3TimeAndPreservesNewest(t *testing.T) {
	for _, mode := range []string{"timestamp", "fixed"} {
		t.Run(mode, func(t *testing.T) {
			a, store, _, _ := newAppTest(t, "full")
			a.Config.FilenameMode = mode
			now := a.now()
			published := a.Config.backupKey("", now)
			store.objects[published], store.modified[published] = []byte("new"), now
			// Even fixed-mode retention must preserve the newest timestamp
			// generations, including ties, although all of them have expired.
			var oldKeys []string
			for i, age := range []time.Duration{72 * time.Hour, 48 * time.Hour, 48 * time.Hour} {
				key := fmt.Sprintf("unit/full/2026-09-10T00:00:00-%032x.zip", i)
				oldKeys = append(oldKeys, key)
				store.objects[key], store.modified[key] = []byte("old"), now.Add(-age)
			}
			// A host clock four minutes ahead must not expire an S3 object
			// whose actual age is still less than the one-day retention period.
			if mode == "timestamp" {
				store.modified[oldKeys[2]] = now.Add(-24*time.Hour + time.Minute)
			}
			if err := a.removeOldBackups(t.Context(), "", published, now.Add(4*time.Minute), now.Add(4*time.Minute), 1); err != nil {
				t.Fatal(err)
			}
			if len(store.objects[published]) == 0 || len(store.objects[oldKeys[2]]) == 0 {
				t.Fatal("published, newest or unexpired generation was deleted")
			}
			wantDeleted := 2
			if mode == "fixed" {
				wantDeleted = 1
				if len(store.objects[oldKeys[1]]) == 0 {
					t.Fatal("a tie for newest timestamp generation was deleted")
				}
			}
			if len(store.deleted) != wantDeleted || len(store.objects[oldKeys[0]]) != 0 {
				t.Fatalf("expired generations not pruned: %v", store.deleted)
			}
		})
	}
}

func TestRetentionValidatesAllMetadataBeforeDeletion(t *testing.T) {
	for _, failure := range []string{"published missing", "published time missing", "old time missing", "clock rollback"} {
		t.Run(failure, func(t *testing.T) {
			a, store, _, _ := newAppTest(t, "full")
			now := a.now()
			published, old := a.Config.backupKey("", now), a.Config.backupKey("", now.Add(-72*time.Hour))
			store.objects[published], store.modified[published] = []byte("new"), now
			store.objects[old], store.modified[old] = []byte("old"), now.Add(-72*time.Hour)
			completed := now
			switch failure {
			case "published missing":
				delete(store.objects, published)
			case "published time missing":
				store.modified[published] = time.Time{}
			case "old time missing":
				store.modified[old] = time.Time{}
			case "clock rollback":
				completed = now.Add(-time.Second)
			}
			if err := a.removeOldBackups(t.Context(), "", published, now, completed, 1); err == nil || len(store.deleted) != 0 {
				t.Fatalf("unsafe retention: err=%v deleted=%v", err, store.deleted)
			}
		})
	}
}

func TestRetentionAcceptsLongMultipartUploadInterval(t *testing.T) {
	a, store, _, _ := newAppTest(t, "full")
	started := a.now()
	published := a.Config.backupKey("", started)
	old := a.Config.backupKey("", started.Add(-72*time.Hour))
	store.objects[published], store.modified[published] = []byte("new"), started
	store.objects[old], store.modified[old] = []byte("old"), started.Add(-72*time.Hour)
	if err := a.removeOldBackups(t.Context(), "", published, started, started.Add(12*time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != old || len(store.objects[published]) == 0 {
		t.Fatalf("long upload retention failed: deleted=%v", store.deleted)
	}
}
