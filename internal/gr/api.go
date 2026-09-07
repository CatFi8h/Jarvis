package gr

// Structured (non-text) API over the watcher, used by the Telegram Mini App
// backend. Command handlers in gr.go render text for the chat UI; these methods
// return data for JSON responses. Both share the same store, validation and
// fetch primitives.

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Direction is one selectable route for the Mini App.
type Direction struct {
	Token string `json:"token"` // "tb" | "bt"
	From  string `json:"from"`
	To    string `json:"to"`
}

// Directions lists the routes the parser supports.
func (w *Watcher) Directions() []Direction {
	return []Direction{
		{Token: "tb", From: stationName(tbilisiCode), To: stationName(batumiCode)},
		{Token: "bt", From: stationName(batumiCode), To: stationName(tbilisiCode)},
	}
}

// SeatClassInfo is one seat class of a train.
type SeatClassInfo struct {
	Name     string  `json:"name"`
	Seats    int     `json:"seats"`
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
}

// TrainInfo is one train in a route+date listing.
type TrainInfo struct {
	Number     int             `json:"number"`
	DepTime    string          `json:"dep_time"` // HH:MM
	ArrTime    string          `json:"arr_time,omitempty"`
	TotalSeats int             `json:"total_seats"`
	SeatsKnown bool            `json:"seats_known"`      // false = no ticket-search data (sales not open?)
	Origin     string          `json:"origin,omitempty"` // set when DepTime is the train's origin, not the searched station
	Classes    []SeatClassInfo `json:"classes"`
	Watched    bool            `json:"watched"` // chat already has a search for it
	WatchID    int             `json:"watch_id,omitempty"`
}

// trainFromRide maps a ticket-search ride to a TrainInfo (seats known).
func trainFromRide(r *ride) TrainInfo {
	total, _ := seatInfo(r)
	t := TrainInfo{
		Number:     r.RideNumber,
		DepTime:    r.hhmm(),
		ArrTime:    r.arrHHMM(),
		TotalSeats: total,
		SeatsKnown: true,
		Classes:    make([]SeatClassInfo, 0, len(r.SeatClasses)),
	}
	for _, sc := range r.SeatClasses {
		nm := sc.SeatClass.Name
		if nm == "" {
			nm = sc.SeatClass.Code
		}
		t.Classes = append(t.Classes, SeatClassInfo{
			Name:     nm,
			Seats:    sc.AvailableNumberOfSeats,
			Price:    sc.PriceOfSeats.Amount,
			Currency: sc.PriceOfSeats.CurrencyCode,
		})
	}
	return t
}

// mergedTrains builds the train list for a route+date: the schedules endpoint
// is the source of truth (it lists trains even before sales open), enriched
// with seats/prices from ticket-search where available. Either source may fail
// independently; only both failing is an error.
func (w *Watcher) mergedTrains(chatID, from, to, date string) ([]TrainInfo, error) {
	sched, schedErr := w.fetchSchedule(from, to, date)
	rides, ridesErr := w.fetchRides(from, to, date)
	if schedErr != nil && ridesErr != nil {
		return nil, fmt.Errorf("fetch failed: %w", ridesErr)
	}

	byNum := map[int]*ride{}
	if ridesErr == nil {
		for i := range rides {
			byNum[rides[i].RideNumber] = &rides[i]
		}
	}

	var trains []TrainInfo
	seen := map[int]bool{}
	for i := range sched {
		e := &sched[i]
		seen[e.RideNumber] = true
		if r, ok := byNum[e.RideNumber]; ok {
			trains = append(trains, trainFromRide(r))
			continue
		}
		t := TrainInfo{
			Number:  e.RideNumber,
			DepTime: localHHMM(e.StartDate),
			ArrTime: localHHMM(e.EndDate),
			Classes: []SeatClassInfo{},
		}
		if e.StartStation.Code != "" && e.StartStation.Code != from {
			t.Origin = e.StartStation.Name // e.g. international train departing Yerevan
		}
		trains = append(trains, t)
	}
	// Rides ticket-search knows but the schedule missed (or schedule fetch failed).
	for i := range rides {
		if !seen[rides[i].RideNumber] {
			trains = append(trains, trainFromRide(&rides[i]))
		}
	}

	sort.Slice(trains, func(i, j int) bool { return trains[i].DepTime < trains[j].DepTime })

	mine := w.store.ForChat(chatID)
	for i := range trains {
		t := &trains[i]
		for j := range mine {
			s := &mine[j]
			if s.FromCode != from || s.Date != date {
				continue
			}
			if (s.TrainNum != "" && s.TrainNum == strconv.Itoa(t.Number)) ||
				(s.TrainNum == "" && s.DepTime != "" && s.DepTime == t.DepTime) {
				t.Watched, t.WatchID = true, s.ID
				break
			}
		}
	}
	return trains, nil
}

// TrainsFor fetches the live train list for a direction token and date.
// chatID marks trains the chat already watches.
func (w *Watcher) TrainsFor(chatID, dir, date string) (route string, trains []TrainInfo, err error) {
	from, to, ok := parseDirection(dir)
	if !ok {
		return "", nil, fmt.Errorf("bad direction %q", dir)
	}
	d, err := parseDate(date)
	if err != nil {
		return "", nil, err
	}
	trains, err = w.mergedTrains(chatID, from, to, d)
	if err != nil {
		return "", nil, err
	}
	return routeLabel(from, to), trains, nil
}

// SearchView is one active search rendered for the Mini App.
type SearchView struct {
	ID         int    `json:"id"`
	Route      string `json:"route"`
	Dir        string `json:"dir"` // "tb" | "bt"
	Date       string `json:"date"`
	Target     string `json:"target"` // "N804 @ 17:10"
	TrainNum   string `json:"train_number,omitempty"`
	DepTime    string `json:"dep_time,omitempty"`
	LastResult string `json:"last_result"`
	Available  bool   `json:"available"`
	BookURL    string `json:"book_url"`
}

func (w *Watcher) searchView(s *Search) SearchView {
	res := s.LastResult
	if res == "" {
		res = "not checked yet"
	}
	return SearchView{
		ID:         s.ID,
		Route:      routeLabel(s.FromCode, s.ToCode),
		Dir:        dirToken(s.FromCode),
		Date:       s.Date,
		Target:     s.targetShort(),
		TrainNum:   s.TrainNum,
		DepTime:    s.DepTime,
		LastResult: res,
		Available:  s.Available,
		BookURL:    w.cfg.bookURL(s.FromCode, s.ToCode, s.Date),
	}
}

// SearchList returns the chat's active searches.
func (w *Watcher) SearchList(chatID string) []SearchView {
	list := w.store.ForChat(chatID)
	out := make([]SearchView, 0, len(list))
	for i := range list {
		out = append(out, w.searchView(&list[i]))
	}
	return out
}

// CreateSearch validates and registers a new watch for the chat, runs an
// immediate check, and returns the search with its current state. The error
// text is user-facing.
func (w *Watcher) CreateSearch(chatID, dir, date, trainNum, depTime string) (SearchView, error) {
	from, to, ok := parseDirection(dir)
	if !ok {
		return SearchView{}, fmt.Errorf("bad direction %q", dir)
	}
	d, err := parseDate(date)
	if err != nil {
		return SearchView{}, err
	}
	if trainNum == "" && depTime == "" {
		return SearchView{}, fmt.Errorf("pick a train (number or departure time)")
	}
	if depTime != "" {
		depTime = normHHMM(depTime)
	}

	for _, ex := range w.store.ForChat(chatID) {
		if ex.FromCode == from && ex.Date == d && ex.DepTime == depTime && ex.TrainNum == trainNum {
			return SearchView{}, fmt.Errorf("you already watch this train (search #%d)", ex.ID)
		}
	}
	if w.store.CountForChat(chatID) >= maxSearchesPerChat {
		return SearchView{}, fmt.Errorf("limit of %d active searches reached — cancel one first", maxSearchesPerChat)
	}

	w.reg.Subscribe(chatID, name)
	s := w.store.Add(Search{
		ChatID:   chatID,
		FromCode: from,
		ToCode:   to,
		Date:     d,
		DepTime:  depTime,
		TrainNum: trainNum,
	})

	rides, ferr := w.fetchRides(from, to, d)
	w.evaluate(s, rides, ferr)

	// Re-read for the updated LastResult/Available.
	for _, cur := range w.store.ForChat(chatID) {
		if cur.ID == s.ID {
			return w.searchView(&cur), nil
		}
	}
	return w.searchView(&s), nil
}

// DeleteSearch cancels one of the chat's searches.
func (w *Watcher) DeleteSearch(chatID string, id int) error {
	if _, ok := w.store.Remove(chatID, id); !ok {
		return fmt.Errorf("search #%d not found", id)
	}
	return nil
}

// CheckSearch polls one search right now and returns its fresh state.
func (w *Watcher) CheckSearch(chatID string, id int) (SearchView, error) {
	for _, s := range w.store.ForChat(chatID) {
		if s.ID == id {
			rides, ferr := w.fetchRides(s.FromCode, s.ToCode, s.Date)
			w.evaluate(s, rides, ferr)
			for _, cur := range w.store.ForChat(chatID) {
				if cur.ID == id {
					return w.searchView(&cur), nil
				}
			}
		}
	}
	return SearchView{}, fmt.Errorf("search #%d not found", id)
}

// MinDate/MaxDate bound the Mini App date picker: today .. today+40d (GR opens
// sales ~30-40 days ahead).
func (w *Watcher) DateRange() (min, max string) {
	now := time.Now()
	return now.Format("2006-01-02"), now.AddDate(0, 0, 40).Format("2006-01-02")
}
