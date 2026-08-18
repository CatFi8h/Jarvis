package gr

import (
	"os"
	"testing"
)

// testdata/response.json is a real gr.com.ge ticket-search response for
// Tbilisi→Batumi: N202 (international, Yerevan→Batumi, departs Tbilisi 00:45)
// and N804 (departs Tbilisi 17:10).
func loadFixture(t *testing.T) []ride {
	t.Helper()
	body, err := os.ReadFile("testdata/response.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	rides, err := parseRides(body)
	if err != nil {
		t.Fatalf("parseRides: %v", err)
	}
	return rides
}

func TestSegmentTimesPreferred(t *testing.T) {
	rides := loadFixture(t)
	if len(rides) != 2 {
		t.Fatalf("want 2 rides, got %d", len(rides))
	}
	// N202's ride origin is Yerevan (dep 14:00) but the searched segment departs
	// Tbilisi at 00:45 — hhmm must report the segment time.
	if got := rides[0].hhmm(); got != "00:45" {
		t.Errorf("N202 hhmm = %q, want 00:45 (Tbilisi departure, not Yerevan)", got)
	}
	if got := rides[1].hhmm(); got != "17:10" {
		t.Errorf("N804 hhmm = %q, want 17:10", got)
	}
	if got := rides[0].arrHHMM(); got != "07:07" {
		t.Errorf("N202 arrHHMM = %q, want 07:07", got)
	}
}

func TestFindRide(t *testing.T) {
	rides := loadFixture(t)
	cases := []struct {
		name    string
		search  Search
		wantNum int
		wantOK  bool
	}{
		{"by number", Search{TrainNum: "804"}, 804, true},
		{"by segment time", Search{DepTime: "17:10"}, 804, true},
		{"by loose time", Search{DepTime: "0:45"}, 202, true},
		{"number+time match", Search{TrainNum: "202", DepTime: "00:45"}, 202, true},
		{"number+wrong time", Search{TrainNum: "202", DepTime: "14:00"}, 0, false}, // Yerevan time must NOT match
		{"missing train", Search{TrainNum: "803"}, 0, false},
	}
	for _, tc := range cases {
		r, ok := findRide(rides, &tc.search)
		if ok != tc.wantOK {
			t.Errorf("%s: ok=%v want %v", tc.name, ok, tc.wantOK)
			continue
		}
		if ok && r.RideNumber != tc.wantNum {
			t.Errorf("%s: got N%d want N%d", tc.name, r.RideNumber, tc.wantNum)
		}
	}
}

func TestParseDirection(t *testing.T) {
	if from, to, ok := parseDirection("tb"); !ok || from != tbilisiCode || to != batumiCode {
		t.Errorf("tb → %s,%s,%v", from, to, ok)
	}
	if from, to, ok := parseDirection("Batumi-Tbilisi"); !ok || from != batumiCode || to != tbilisiCode {
		t.Errorf("Batumi-Tbilisi → %s,%s,%v", from, to, ok)
	}
	if _, _, ok := parseDirection("x"); ok {
		t.Error("x should not parse")
	}
}

func TestParseTarget(t *testing.T) {
	if tm, num, err := parseTarget([]string{"6:00"}); err != nil || tm != "06:00" || num != "" {
		t.Errorf("6:00 → %q %q %v", tm, num, err)
	}
	if tm, num, err := parseTarget([]string{"803"}); err != nil || tm != "" || num != "803" {
		t.Errorf("803 → %q %q %v", tm, num, err)
	}
	if tm, num, err := parseTarget([]string{"N803", "08:00"}); err != nil || tm != "08:00" || num != "803" {
		t.Errorf("N803 08:00 → %q %q %v", tm, num, err)
	}
	if _, _, err := parseTarget([]string{}); err == nil {
		t.Error("empty target should error")
	}
	if _, _, err := parseTarget([]string{"morning"}); err == nil {
		t.Error("junk target should error")
	}
}

func TestStoreLifecycle(t *testing.T) {
	path := t.TempDir() + "/searches.json"
	st := newStore(path)

	a := st.Add(Search{ChatID: "1", FromCode: tbilisiCode, ToCode: batumiCode, Date: "2030-01-02", DepTime: "06:00"})
	b := st.Add(Search{ChatID: "1", FromCode: tbilisiCode, ToCode: batumiCode, Date: "2030-01-02", DepTime: "08:00"})
	c := st.Add(Search{ChatID: "2", FromCode: batumiCode, ToCode: tbilisiCode, Date: "2030-01-03", TrainNum: "802"})
	if a.ID == b.ID || b.ID == c.ID {
		t.Fatal("ids must be unique")
	}
	if got := st.CountForChat("1"); got != 2 {
		t.Errorf("chat 1 count = %d, want 2", got)
	}

	// Persistence: reload from disk.
	st2 := newStore(path)
	if got := len(st2.Snapshot()); got != 3 {
		t.Fatalf("reloaded %d searches, want 3", got)
	}
	d := st2.Add(Search{ChatID: "2", FromCode: batumiCode, ToCode: tbilisiCode, Date: "2030-01-04", DepTime: "14:00"})
	if d.ID <= c.ID {
		t.Errorf("id after reload = %d, must be > %d", d.ID, c.ID)
	}

	// A chat can only remove its own searches.
	if _, ok := st2.Remove("1", c.ID); ok {
		t.Error("chat 1 must not remove chat 2's search")
	}
	if _, ok := st2.Remove("1", a.ID); !ok {
		t.Error("chat 1 should remove its own search")
	}
	exp := st2.ExpireBefore("2030-01-04")
	if len(exp) != 2 { // b (01-02) and c (01-03)
		t.Errorf("expired %d, want 2", len(exp))
	}
	if n := st2.RemoveAll("2"); n != 1 {
		t.Errorf("RemoveAll chat 2 = %d, want 1", n)
	}
}

func TestApplyOverridesRewritesBody(t *testing.T) {
	c := &Config{Passengers: "1", Child: "0", Disabled: "0"}
	r := &request{
		method: "POST",
		url:    "https://gr.com.ge/api/ticket-search",
		body:   `{"startStationCode":"57151","endStationCode":"56014","departureDateFrom":"2026-08-01","routeType":0}`,
	}
	out := applyOverrides(c, r, tbilisiCode, batumiCode, "2026-09-15")
	want := `{"startStationCode":"56014","endStationCode":"57151","departureDateFrom":"2026-09-15","routeType":0}`
	if out.body != want {
		t.Errorf("body = %s\nwant %s", out.body, want)
	}
}
