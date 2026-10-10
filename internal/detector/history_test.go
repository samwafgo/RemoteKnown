package detector

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"RemoteKnown/internal/storage"
)

func TestHistoryDetectorPaginatedCompatibility(t *testing.T) {
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	end := base.Add(time.Minute)
	for _, session := range []storage.RemoteSession{
		{ID: "ended", StartTime: base, EndTime: &end, Duration: 60, Signals: "ToDesk"},
		{ID: "active", StartTime: base.Add(time.Hour), Signals: "ToDesk"},
	} {
		if err := store.SaveSession(&session); err != nil {
			t.Fatal(err)
		}
	}
	// History calls do not need to start the Windows detection loop.
	d := &Detector{storage: store}
	old, oldTotal, err := d.GetHistoryPaginated(1, 5)
	if err != nil {
		t.Fatal(err)
	}
	empty, total, err := d.GetHistoryPaginatedFiltered(1, 5, storage.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old, empty) || oldTotal != 2 || total != oldTotal {
		t.Fatalf("legacy pagination differs: %v/%d vs %v/%d", old, oldTotal, empty, total)
	}
	minimum := int64(0)
	filtered, total, err := d.GetHistoryPaginatedFiltered(1, 5, storage.SessionFilter{MinDuration: &minimum, Keyword: "Desk"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(filtered) != 1 || filtered[0].ID != "ended" {
		t.Fatalf("filtered forwarding failed: %v/%d", filtered, total)
	}
}
