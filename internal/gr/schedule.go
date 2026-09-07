package gr

// The schedules/core endpoint returns the full timetable for a direction+date,
// including trains whose ticket sales haven't opened yet — unlike ticket-search,
// which only knows rides with sales data. Listings use this as the source of
// truth and enrich with ticket-search seats; the watch poll loop stays on
// ticket-search (it needs seat counts).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const defaultScheduleEndpoint = "https://gr.com.ge/api/schedules/core"

// Numeric station ids used by schedules/core (distinct from the 5-digit codes
// used by ticket-search).
const (
	tbilisiID = "98"
	batumiID  = "309"
)

func stationID(code string) string {
	switch code {
	case tbilisiCode:
		return tbilisiID
	case batumiCode:
		return batumiID
	}
	return ""
}

// scheduleEntry is one train in a schedules/core response.
type scheduleEntry struct {
	RideNumber int `json:"rideNumber"`
	RouteType  struct {
		Code string `json:"code"` // "local" | "international"
	} `json:"routeType"`
	StartDate    string `json:"startDate"` // RFC3339 UTC, origin departure
	EndDate      string `json:"endDate"`   // RFC3339 UTC, final arrival
	StartStation struct {
		ID   int    `json:"id"`
		Code string `json:"code"`
		Name string `json:"name"`
	} `json:"startStation"`
	EndStation struct {
		ID   int    `json:"id"`
		Code string `json:"code"`
		Name string `json:"name"`
	} `json:"endStation"`
}

func parseSchedule(body []byte) ([]scheduleEntry, error) {
	var payload struct {
		List []scheduleEntry `json:"list"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("unexpected schedule response shape: %w", err)
	}
	return payload.List, nil
}

// georgiaTZ is the timezone schedule times are displayed in. Schedule
// startDate/endDate come as UTC instants.
var georgiaTZ = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Tbilisi"); err == nil {
		return loc
	}
	return time.FixedZone("+04", 4*3600)
}()

// localHHMM converts an RFC3339 UTC schedule timestamp to local HH:MM
// ("" on parse failure).
func localHHMM(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return ""
	}
	return t.In(georgiaTZ).Format("15:04")
}

// fetchSchedule fetches the timetable for a route+date from schedules/core.
func (w *Watcher) fetchSchedule(fromCode, toCode, date string) ([]scheduleEntry, error) {
	fromID, toID := stationID(fromCode), stationID(toCode)
	if fromID == "" || toID == "" {
		return nil, fmt.Errorf("no schedule station id for route %s→%s", fromCode, toCode)
	}
	q := url.Values{}
	q.Set("startStationId", fromID)
	q.Set("endStationId", toID)
	q.Set("startDate", date)
	body, err := fetch(w.client, &request{
		method:  http.MethodGet,
		url:     w.cfg.ScheduleEndpoint + "?" + q.Encode(),
		headers: defaultHeaders(),
	})
	if err != nil {
		return nil, err
	}
	return parseSchedule(body)
}
