package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"RemoteKnown/internal/detector"
	"RemoteKnown/internal/storage"
)

func newHistoryServer(t *testing.T) *Server {
	t.Helper()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id       string
		start    time.Time
		duration int64
		signals  string
		active   bool
	}{
		{"old", base.Add(-time.Hour), 0, "ToDesk", false},
		{"offset", base.In(time.FixedZone("UTC+08", 8*60*60)), 60, "ToDesk%", false},
		{"utc", base, 120, "ToDesk_, 向日葵", false},
		{"slash", base.Add(time.Hour), 30, `Remote\A`, false},
		{"injection", base.Add(2 * time.Hour), 1, "' OR 1=1 --", false},
		{"active", base.Add(3 * time.Hour), 0, "ToDesk", true},
	} {
		session := storage.RemoteSession{ID: row.id, StartTime: row.start, Duration: row.duration, Signals: row.signals}
		if !row.active {
			end := row.start.Add(time.Duration(row.duration) * time.Second)
			session.EndTime = &end
		}
		if err := store.SaveSession(&session); err != nil {
			t.Fatal(err)
		}
	}
	return NewServer(detector.NewDetector(store, nil), store, nil, nil)
}

func TestHistoryAPIFiltersAndPagination(t *testing.T) {
	srv := newHistoryServer(t)
	for _, tc := range []struct {
		name       string
		query      url.Values
		ids        []string
		total      int64
		page       int
		pageSize   int
		totalPages int
	}{
		{"legacy defaults", nil, []string{"active", "injection", "slash", "utc", "offset"}, 6, 1, 5, 2},
		{"legacy second page", url.Values{"page": {"2"}, "pageSize": {"5"}}, []string{"old"}, 6, 2, 5, 2},
		{"legacy invalid pagination falls back", url.Values{"page": {"bad"}, "pageSize": {"-3"}}, []string{"active", "injection", "slash", "utc", "offset"}, 6, 1, 5, 2},
		{"legacy zero pagination falls back", url.Values{"page": {"0"}, "pageSize": {"0"}}, []string{"active", "injection", "slash", "utc", "offset"}, 6, 1, 5, 2},
		{"UTC lower bound", url.Values{"start_from": {"2026-10-10T00:00:00Z"}}, []string{"active", "injection", "slash", "utc", "offset"}, 5, 1, 5, 1},
		{"offset upper bound", url.Values{"start_to": {"2026-10-10T08:00:00+08:00"}}, []string{"utc", "offset", "old"}, 3, 1, 5, 1},
		{"equal offset normalized bounds", url.Values{"start_from": {"2026-10-10T08:00:00+08:00"}, "start_to": {"2026-10-10T00:00:00Z"}}, []string{"utc", "offset"}, 2, 1, 5, 1},
		{"fractional lower bound", url.Values{"start_from": {"2026-10-10T00:00:00.500Z"}}, []string{"active", "injection", "slash"}, 3, 1, 5, 1},
		{"duration inclusive minimum", url.Values{"min_duration": {"60"}}, []string{"utc", "offset"}, 2, 1, 5, 1},
		{"duration inclusive maximum", url.Values{"max_duration": {"30"}}, []string{"injection", "slash", "old"}, 3, 1, 5, 1},
		{"zero duration excludes active", url.Values{"min_duration": {"0"}, "max_duration": {"0"}}, []string{"old"}, 1, 1, 5, 1},
		{"maximum int64", url.Values{"max_duration": {"9223372036854775807"}}, []string{"injection", "slash", "utc", "offset", "old"}, 5, 1, 5, 1},
		{"equal duration bounds", url.Values{"min_duration": {"60"}, "max_duration": {"60"}}, []string{"offset"}, 1, 1, 5, 1},
		{"keyword active", url.Values{"keyword": {"Desk"}}, []string{"active", "utc", "offset", "old"}, 4, 1, 5, 1},
		{"Chinese keyword", url.Values{"keyword": {"向日葵"}}, []string{"utc"}, 1, 1, 5, 1},
		{"literal percent", url.Values{"keyword": {"%"}}, []string{"offset"}, 1, 1, 5, 1},
		{"literal underscore", url.Values{"keyword": {"_"}}, []string{"utc"}, 1, 1, 5, 1},
		{"literal backslash", url.Values{"keyword": {`\`}}, []string{"slash"}, 1, 1, 5, 1},
		{"literal SQL injection", url.Values{"keyword": {"' OR 1=1 --"}}, []string{"injection"}, 1, 1, 5, 1},
		{"SQL injection cannot broaden results", url.Values{"keyword": {"') OR 1=1 --"}}, []string{}, 0, 1, 5, 0},
		{"combined page and count", url.Values{"start_from": {"2026-10-10T08:00:00+08:00"}, "start_to": {"2026-10-10T01:00:00Z"}, "min_duration": {"60"}, "max_duration": {"120"}, "keyword": {"ToDesk"}, "page": {"2"}, "pageSize": {"1"}}, []string{"offset"}, 2, 2, 1, 2},
		{"empty page keeps filtered total", url.Values{"keyword": {"ToDesk"}, "page": {"5"}, "pageSize": {"1"}}, []string{}, 4, 5, 1, 4},
		{"empty keyword no filter", url.Values{"keyword": {""}}, []string{"active", "injection", "slash", "utc", "offset"}, 6, 1, 5, 2},
		{"keyword spaces are literal", url.Values{"keyword": {" ToDesk "}}, []string{}, 0, 1, 5, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.handleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/history?"+tc.query.Encode(), nil))
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("status/type/body = %d/%s/%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
			}
			var body struct {
				Sessions   []storage.RemoteSession `json:"sessions"`
				Total      int64                   `json:"total"`
				Page       int                     `json:"page"`
				PageSize   int                     `json:"pageSize"`
				TotalPages int                     `json:"totalPages"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			ids := make([]string, len(body.Sessions))
			for i, session := range body.Sessions {
				ids[i] = session.ID
				if session.ID == "active" && session.EndTime != nil {
					t.Error("active end_time must remain null")
				}
			}
			if !reflect.DeepEqual(ids, tc.ids) || body.Total != tc.total || body.Page != tc.page || body.PageSize != tc.pageSize || body.TotalPages != tc.totalPages {
				t.Fatalf("body = %+v (ids %v), want ids/total/page/pageSize/totalPages = %v/%d/%d/%d/%d", body, ids, tc.ids, tc.total, tc.page, tc.pageSize, tc.totalPages)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 5 {
				t.Fatalf("response fields changed: %v", fields)
			}
			if strings.TrimSpace(string(fields["sessions"])) == "null" {
				t.Error("sessions must be an array even when empty")
			}
		})
	}
}

func TestHistoryAPIInvalidFilters(t *testing.T) {
	// Invalid filters must be rejected before consulting the detector/database.
	srv := NewServer(nil, nil, nil, nil)
	for _, tc := range []struct {
		name  string
		query url.Values
	}{
		{"invalid from", url.Values{"start_from": {"not-a-date"}}},
		{"invalid to", url.Values{"start_to": {"2026-02-30T00:00:00Z"}}},
		{"empty from", url.Values{"start_from": {""}}},
		{"empty to", url.Values{"start_to": {""}}},
		{"missing zone", url.Values{"start_from": {"2026-10-10T00:00:00"}}},
		{"date only", url.Values{"start_to": {"2026-10-10"}}},
		{"non RFC3339 single digit hour", url.Values{"start_from": {"2026-10-10T1:00:00Z"}}},
		{"non RFC3339 decimal comma", url.Values{"start_to": {"2026-10-10T00:00:00,5Z"}}},
		{"offset hour out of range", url.Values{"start_from": {"2026-10-10T00:00:00+24:00"}}},
		{"offset minute out of range", url.Values{"start_to": {"2026-10-10T00:00:00+08:60"}}},
		{"inverted dates normalized", url.Values{"start_from": {"2026-10-10T09:00:00+08:00"}, "start_to": {"2026-10-10T00:00:00Z"}}},
		{"negative min", url.Values{"min_duration": {"-1"}}},
		{"negative max", url.Values{"max_duration": {"-1"}}},
		{"empty min", url.Values{"min_duration": {""}}},
		{"empty max", url.Values{"max_duration": {""}}},
		{"decimal min", url.Values{"min_duration": {"1.5"}}},
		{"decimal max", url.Values{"max_duration": {"0.5"}}},
		{"not integer", url.Values{"min_duration": {"NaN"}}},
		{"overflow min", url.Values{"min_duration": {"9223372036854775808"}}},
		{"overflow max", url.Values{"max_duration": {"9223372036854775808"}}},
		{"inverted durations", url.Values{"min_duration": {"60"}, "max_duration": {"30"}}},
		{"duplicate start", url.Values{"start_from": {"2026-10-10T00:00:00Z", "2026-10-10T01:00:00Z"}}},
		{"duplicate duration", url.Values{"min_duration": {"0", "60"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.handleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/history?"+tc.query.Encode(), nil))
			if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("status/type = %d/%s, body %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
			}
			var body struct {
				Success bool   `json:"success"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Success || !strings.ContainsFunc(body.Error, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
				t.Fatalf("expected success=false and Chinese error, got %+v", body)
			}
		})
	}
}

func TestHistoryAPIMalformedQuery(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil)
	for _, query := range []string{"start_from=%ZZ", "min_duration=%", "keyword=%0X", "start_from=invalid;start_to=invalid"} {
		t.Run(query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.handleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/history?"+query, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["success"] != false || body["error"] == "" {
				t.Fatalf("invalid error response: %v", body)
			}
		})
	}
}

func TestHistoryAPIMethodNotAllowed(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.handleHistory(rec, httptest.NewRequest(http.MethodPost, "/api/history", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
