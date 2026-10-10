package storage

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func newHistoryStorage(t *testing.T) (*Storage, time.Time) {
	t.Helper()
	store, err := NewStorage(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id       string
		start    time.Time
		duration int64
		signals  string
		active   bool
	}{
		{"a-old", base.Add(-time.Hour), 0, "ToDesk", false},
		{"b-offset", base.In(time.FixedZone("UTC+08", 8*60*60)), 60, "ToDesk", false},
		{"c-utc", base, 120, "ToDesk, 向日葵", false},
		{"d-percent", base.Add(time.Hour), 30, "Remote100%", false},
		{"e-underscore", base.Add(2 * time.Hour), 60, "Remote_A", false},
		{"f-backslash", base.Add(3 * time.Hour), 60, `Remote\A`, false},
		{"g-injection", base.Add(4 * time.Hour), 1, "' OR 1=1 --", false},
		{"h-active", base.Add(5 * time.Hour), 0, "ToDesk", true},
	} {
		session := RemoteSession{ID: row.id, StartTime: row.start, Duration: row.duration, Signals: row.signals}
		if !row.active {
			end := row.start.Add(time.Duration(row.duration) * time.Second)
			session.EndTime = &end
		}
		if err := store.SaveSession(&session); err != nil {
			t.Fatal(err)
		}
	}
	return store, base
}

func historyTime(value time.Time) *time.Time { return &value }
func historySeconds(value int64) *int64      { return &value }

func TestHistoryStorageFilters(t *testing.T) {
	store, base := newHistoryStorage(t)
	all := []string{"h-active", "g-injection", "f-backslash", "e-underscore", "d-percent", "c-utc", "b-offset", "a-old"}
	for _, tc := range []struct {
		name     string
		filter   SessionFilter
		page     int
		pageSize int
		ids      []string
		total    int64
	}{
		{"unfiltered", SessionFilter{}, 1, 20, all, 8},
		{"from inclusive UTC", SessionFilter{StartFrom: historyTime(base)}, 1, 20, all[:7], 7},
		{"to inclusive UTC", SessionFilter{StartTo: historyTime(base)}, 1, 20, []string{"c-utc", "b-offset", "a-old"}, 3},
		{"equal mixed offset bounds", SessionFilter{StartFrom: historyTime(base.In(time.FixedZone("UTC+08", 8*60*60))), StartTo: historyTime(base)}, 1, 20, []string{"c-utc", "b-offset"}, 2},
		{"after offset boundary", SessionFilter{StartFrom: historyTime(base.Add(time.Second))}, 1, 20, all[:5], 5},
		{"before offset boundary", SessionFilter{StartTo: historyTime(base.Add(-time.Second))}, 1, 20, []string{"a-old"}, 1},
		{"minimum seconds", SessionFilter{MinDuration: historySeconds(60)}, 1, 20, []string{"f-backslash", "e-underscore", "c-utc", "b-offset"}, 4},
		{"maximum seconds", SessionFilter{MaxDuration: historySeconds(30)}, 1, 20, []string{"g-injection", "d-percent", "a-old"}, 3},
		{"equal duration bounds", SessionFilter{MinDuration: historySeconds(60), MaxDuration: historySeconds(60)}, 1, 20, []string{"f-backslash", "e-underscore", "b-offset"}, 3},
		{"zero minimum excludes active", SessionFilter{MinDuration: historySeconds(0)}, 1, 20, all[1:], 7},
		{"zero maximum includes ended zero", SessionFilter{MaxDuration: historySeconds(0)}, 1, 20, []string{"a-old"}, 1},
		{"keyword includes active", SessionFilter{Keyword: "Desk"}, 1, 20, []string{"h-active", "c-utc", "b-offset", "a-old"}, 4},
		{"Chinese keyword", SessionFilter{Keyword: "向日葵"}, 1, 20, []string{"c-utc"}, 1},
		{"literal percent", SessionFilter{Keyword: "%"}, 1, 20, []string{"d-percent"}, 1},
		{"literal underscore", SessionFilter{Keyword: "_"}, 1, 20, []string{"e-underscore"}, 1},
		{"literal backslash", SessionFilter{Keyword: `\`}, 1, 20, []string{"f-backslash"}, 1},
		{"mixed metacharacters", SessionFilter{Keyword: `%_\`}, 1, 20, []string{}, 0},
		{"literal SQL injection", SessionFilter{Keyword: "' OR 1=1 --"}, 1, 20, []string{"g-injection"}, 1},
		{"SQL cannot broaden results", SessionFilter{Keyword: "') OR 1=1 --"}, 1, 20, []string{}, 0},
		{"combined second page", SessionFilter{StartFrom: historyTime(base), StartTo: historyTime(base.Add(2 * time.Hour)), MinDuration: historySeconds(60), MaxDuration: historySeconds(120), Keyword: "ToDesk"}, 2, 1, []string{"b-offset"}, 2},
		{"combined first page", SessionFilter{StartFrom: historyTime(base), StartTo: historyTime(base.Add(2 * time.Hour)), MinDuration: historySeconds(60), MaxDuration: historySeconds(120), Keyword: "ToDesk"}, 1, 1, []string{"c-utc"}, 2},
		{"past last page keeps total", SessionFilter{Keyword: "ToDesk"}, 5, 1, []string{}, 4},
		{"int64 maximum", SessionFilter{MaxDuration: historySeconds(1<<63 - 1)}, 1, 20, all[1:], 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, total, err := store.GetSessionsPaginatedFiltered(tc.page, tc.pageSize, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, len(sessions))
			for i, session := range sessions {
				ids[i] = session.ID
			}
			if !reflect.DeepEqual(ids, tc.ids) || total != tc.total {
				t.Fatalf("ids/total = %v/%d, want %v/%d", ids, total, tc.ids, tc.total)
			}
		})
	}
}

func TestHistoryStorageLegacyPagination(t *testing.T) {
	store, _ := newHistoryStorage(t)
	old, oldTotal, err := store.GetSessionsPaginated(2, 3)
	if err != nil {
		t.Fatal(err)
	}
	filtered, total, err := store.GetSessionsPaginatedFiltered(2, 3, SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old, filtered) || oldTotal != total || total != 8 {
		t.Fatalf("legacy and empty-filter results differ: %v/%d vs %v/%d", old, oldTotal, filtered, total)
	}
	if len(old) != 3 || old[0].ID != "e-underscore" || old[2].ID != "c-utc" {
		t.Fatalf("unexpected legacy page: %v", old)
	}
}

func TestHistoryStorageInvalidFilters(t *testing.T) {
	store, base := newHistoryStorage(t)
	for _, tc := range []struct {
		name   string
		filter SessionFilter
	}{
		{"inverted time", SessionFilter{StartFrom: historyTime(base.Add(time.Second)), StartTo: historyTime(base)}},
		{"negative minimum", SessionFilter{MinDuration: historySeconds(-1)}},
		{"negative maximum", SessionFilter{MaxDuration: historySeconds(-1)}},
		{"inverted duration", SessionFilter{MinDuration: historySeconds(2), MaxDuration: historySeconds(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := store.GetSessionsPaginatedFiltered(1, 5, tc.filter); err == nil {
				t.Fatal("invalid filter accepted")
			}
		})
	}
}
