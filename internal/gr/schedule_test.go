package gr

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testdata/schedules.json is a real schedules/core response for
// Tbilisi→Batumi 2026-09-08: N202 (international, from Yerevan), N802, N804,
// N806, N808. Times are UTC instants (Georgia is UTC+4).
func loadSchedule(t *testing.T) []scheduleEntry {
	t.Helper()
	body, err := os.ReadFile("testdata/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	list, err := parseSchedule(body)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestParseSchedule(t *testing.T) {
	list := loadSchedule(t)
	if len(list) != 5 {
		t.Fatalf("got %d entries, want 5", len(list))
	}
	nums := map[int]bool{}
	for _, e := range list {
		nums[e.RideNumber] = true
	}
	for _, want := range []int{202, 802, 804, 806, 808} {
		if !nums[want] {
			t.Errorf("ride N%d missing from parsed schedule", want)
		}
	}
}

func TestLocalHHMM(t *testing.T) {
	// N804 departs 13:10 UTC = 17:10 Tbilisi.
	if got := localHHMM("2026-09-08T13:10:00Z"); got != "17:10" {
		t.Errorf("localHHMM = %q, want 17:10", got)
	}
	if got := localHHMM("garbage"); got != "" {
		t.Errorf("localHHMM(garbage) = %q, want empty", got)
	}
}

// newMergeWatcher builds a watcher whose schedule endpoint and ticket-search
// endpoint are served from the two fixtures.
func newMergeWatcher(t *testing.T) *Watcher {
	t.Helper()
	sched, err := os.ReadFile("testdata/schedules.json")
	if err != nil {
		t.Fatal(err)
	}
	rides, err := os.ReadFile("testdata/response.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/schedules":
			w.Write(sched)
		default:
			w.Write(rides)
		}
	}))
	t.Cleanup(srv.Close)

	return &Watcher{
		cfg: &Config{
			APIEndpoint:      srv.URL + "/search",
			ScheduleEndpoint: srv.URL + "/schedules",
			CurlFile:         filepath.Join(t.TempDir(), "absent.curl"),
			Passengers:       "1",
		},
		client: &http.Client{Timeout: 5 * time.Second},
		store:  newStore(filepath.Join(t.TempDir(), "s.json")),
	}
}

func TestMergedTrains(t *testing.T) {
	w := newMergeWatcher(t)
	trains, err := w.mergedTrains("chat1", tbilisiCode, batumiCode, "2026-09-08")
	if err != nil {
		t.Fatal(err)
	}
	if len(trains) != 5 {
		t.Fatalf("got %d trains, want 5 (full schedule)", len(trains))
	}

	byNum := map[int]TrainInfo{}
	for _, tr := range trains {
		byNum[tr.Number] = tr
	}

	// N804 exists in both sources → ticket-search wins: segment times + seats.
	n804 := byNum[804]
	if !n804.SeatsKnown {
		t.Error("N804 should have known seats (present in ticket-search)")
	}
	if n804.DepTime != "17:10" {
		t.Errorf("N804 dep = %q, want 17:10", n804.DepTime)
	}
	// Seat merging: N202 has Econom:1 in the ticket-search fixture.
	if n202 := byNum[202]; n202.TotalSeats != 1 || len(n202.Classes) != 1 {
		t.Errorf("N202 seats not merged: total=%d classes=%d", n202.TotalSeats, len(n202.Classes))
	}

	// N802 is schedule-only → seats unknown, UTC 04:05 → local 08:05.
	n802 := byNum[802]
	if n802.SeatsKnown {
		t.Error("N802 should be seats-unknown (not in ticket-search fixture)")
	}
	if n802.DepTime != "08:05" {
		t.Errorf("N802 dep = %q, want 08:05", n802.DepTime)
	}

	// N202 is in both sources (international): ticket-search segment data wins,
	// so no origin label; seats known.
	if n202 := byNum[202]; !n202.SeatsKnown {
		t.Error("N202 should have known seats (present in ticket-search)")
	}

	// Sorted by departure time.
	for i := 1; i < len(trains); i++ {
		if trains[i-1].DepTime > trains[i].DepTime {
			t.Fatalf("trains not sorted by DepTime: %v", trains)
		}
	}
}

func TestMergedTrainsOriginLabel(t *testing.T) {
	// Schedule-only international entry (empty ticket-search) → origin set.
	w := newMergeWatcher(t)
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte("[[]]"))
	}))
	defer srvEmpty.Close()
	w.cfg.APIEndpoint = srvEmpty.URL

	trains, err := w.mergedTrains("chat1", tbilisiCode, batumiCode, "2026-09-08")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trains {
		if tr.Number == 202 {
			if tr.Origin != "Yerevan" {
				t.Errorf("N202 origin = %q, want Yerevan", tr.Origin)
			}
			if tr.SeatsKnown {
				t.Error("N202 should be seats-unknown with empty ticket-search")
			}
			return
		}
	}
	t.Fatal("N202 not in merged list")
}

func TestMergedTrainsScheduleDown(t *testing.T) {
	// Schedule endpoint failing → fall back to ticket-search-only list.
	w := newMergeWatcher(t)
	w.cfg.ScheduleEndpoint = "http://127.0.0.1:1/nope"
	trains, err := w.mergedTrains("chat1", tbilisiCode, batumiCode, "2026-09-08")
	if err != nil {
		t.Fatal(err)
	}
	if len(trains) != 2 { // response.json has N202 + N804
		t.Fatalf("got %d trains, want 2 from ticket-search fallback", len(trains))
	}
	for _, tr := range trains {
		if !tr.SeatsKnown {
			t.Errorf("N%d should have known seats in fallback", tr.Number)
		}
	}
}

func TestMergedTrainsWatchedMark(t *testing.T) {
	w := newMergeWatcher(t)
	w.store.Add(Search{ChatID: "chat1", FromCode: tbilisiCode, ToCode: batumiCode,
		Date: "2026-09-08", TrainNum: "804", DepTime: "17:10"})

	trains, err := w.mergedTrains("chat1", tbilisiCode, batumiCode, "2026-09-08")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trains {
		want := tr.Number == 804
		if tr.Watched != want {
			t.Errorf("N%d watched = %v, want %v", tr.Number, tr.Watched, want)
		}
	}
}
