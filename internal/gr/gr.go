// Package gr is the Georgian Railway ticket parser: it polls gr.com.ge for free
// seats on specific trains and alerts the chat that asked the moment one appears.
//
// Unlike the old single-search setup, every chat can run several concurrent
// searches (e.g. Tbilisi→Batumi 06:00 and 08:00, plus Batumi→Tbilisi next day),
// each created from the bot with /gr_search and persisted across restarts.
// .env only supplies defaults (poll interval, passengers, endpoint overrides) —
// see .env.example. Optionally a captured "Copy as cURL" request (API_CURL_FILE,
// default request.curl) supplies auth cookies/headers; otherwise the known
// default endpoint is used.
package gr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"jarvis-bot/internal/subs"
	"jarvis-bot/internal/tg"
)

const (
	name            = "gr"
	defaultEndpoint = "https://gr.com.ge/api/ticket-search"
)

// ─────────────────────────────── stations ──────────────────────────────────

const (
	tbilisiCode = "56014"
	batumiCode  = "57151"
)

func stationName(code string) string {
	switch code {
	case tbilisiCode:
		return "Tbilisi"
	case batumiCode:
		return "Batumi"
	}
	return code
}

// parseDirection maps a user token to (from, to) station codes.
// "tb"/"tbilisi"/"tbilisi-batumi" → Tbilisi→Batumi; "bt"/"batumi"/... → back.
func parseDirection(tok string) (from, to string, ok bool) {
	t := strings.ToLower(strings.TrimSpace(tok))
	switch {
	case t == "":
		return "", "", false
	case strings.HasPrefix(t, "t"):
		return tbilisiCode, batumiCode, true
	case strings.HasPrefix(t, "b"):
		return batumiCode, tbilisiCode, true
	}
	return "", "", false
}

func routeLabel(from, to string) string {
	return stationName(from) + "→" + stationName(to)
}

// ─────────────────────────────── config ────────────────────────────────────

type Config struct {
	Passengers string
	Child      string
	Disabled   string

	Interval      time.Duration
	AlertRepeat   time.Duration
	HeartbeatEver time.Duration

	APIEndpoint string
	RouteType   int

	SearchesFile string

	CurlFile   string
	APIURL     string
	APIMethod  string
	APIBody    string
	APIHeaders map[string]string
}

func loadConfig() (*Config, error) {
	c := &Config{
		Passengers:   env("PASSENGERS", "1"),
		Child:        env("CHILD_PASSENGERS", "0"),
		Disabled:     env("DISABLED_PASSENGERS", "0"),
		APIEndpoint:  env("API_ENDPOINT", defaultEndpoint),
		SearchesFile: env("GR_SEARCHES_FILE", "gr_searches.json"),
		CurlFile:     env("API_CURL_FILE", "request.curl"),
		APIURL:       env("API_URL", ""),
		APIMethod:    env("API_METHOD", ""),
		APIBody:      env("API_BODY", ""),
		APIHeaders:   map[string]string{},
	}
	c.RouteType = atoiDefault(env("ROUTE_TYPE", "0"), 0)
	c.Interval = envDur("CHECK_INTERVAL", 5*time.Minute)
	c.AlertRepeat = envDur("ALERT_REPEAT", 30*time.Minute)
	c.HeartbeatEver = envDur("HEARTBEAT_EVERY", 0) // 0 = off
	if v := env("API_HEADERS_JSON", ""); v != "" {
		_ = json.Unmarshal([]byte(v), &c.APIHeaders)
	}

	if c.Interval < time.Minute {
		c.Interval = time.Minute // be polite to the site
	}
	return c, nil
}

func (c *Config) bookURL(from, to, date string) string {
	return fmt.Sprintf("https://gr.com.ge/en/search?startStationCode=%s&endStationCode=%s"+
		"&departureDateFrom=%s&standard_passengers=%s&child_passengers=%s&disabled_passengers=%s",
		from, to, date, c.Passengers, c.Child, c.Disabled)
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	v := env(k, "")
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil { // bare number = minutes
		return time.Duration(n) * time.Minute
	}
	return def
}

// ─────────────────────────── request building ──────────────────────────────

type request struct {
	method  string
	url     string
	headers map[string]string
	body    string
}

var dateRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

type searchBody struct {
	StartStationCode   string `json:"startStationCode"`
	EndStationCode     string `json:"endStationCode"`
	DepartureDateFrom  string `json:"departureDateFrom"`
	StandardPassengers int    `json:"standard_passengers"`
	ChildPassengers    int    `json:"child_passengers"`
	DisabledPassengers int    `json:"disabled_passengers"`
	RouteType          int    `json:"routeType"`
}

// buildRequest constructs a poll request for one route+date. Priority:
// (1) captured cURL in CurlFile, (2) explicit API_URL + API_BODY, (3) the known
// default POST. Captured/explicit requests get their station codes and date
// rewritten to the requested route.
func buildRequest(c *Config, from, to, date string) (*request, error) {
	if raw, err := os.ReadFile(c.CurlFile); err == nil {
		if curl := stripComments(string(raw)); strings.Contains(curl, "http") {
			r := parseCurl(curl)
			return applyOverrides(c, r, from, to, date), nil
		}
	}

	if c.APIURL != "" {
		method := c.APIMethod
		if method == "" {
			if c.APIBody != "" {
				method = http.MethodPost
			} else {
				method = http.MethodGet
			}
		}
		r := &request{method: method, url: c.APIURL, headers: defaultHeaders(), body: c.APIBody}
		for k, v := range c.APIHeaders {
			r.headers[k] = v
		}
		return applyOverrides(c, r, from, to, date), nil
	}

	body, err := json.Marshal(searchBody{
		StartStationCode:   from,
		EndStationCode:     to,
		DepartureDateFrom:  date,
		StandardPassengers: atoiDefault(c.Passengers, 1),
		ChildPassengers:    atoiDefault(c.Child, 0),
		DisabledPassengers: atoiDefault(c.Disabled, 0),
		RouteType:          c.RouteType,
	})
	if err != nil {
		return nil, err
	}
	return &request{
		method:  http.MethodPost,
		url:     c.APIEndpoint,
		headers: defaultHeaders(),
		body:    string(body),
	}, nil
}

func defaultHeaders() map[string]string {
	return map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"User-Agent":   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
		"Origin":       "https://gr.com.ge",
		"Referer":      "https://gr.com.ge/en/search",
	}
}

// applyOverrides forces the target route/date onto a captured request: dates via
// regex, station codes via query params and JSON body fields.
func applyOverrides(c *Config, r *request, from, to, date string) *request {
	r.url = dateRe.ReplaceAllString(r.url, date)
	r.body = dateRe.ReplaceAllString(r.body, date)
	r.body = setJSONField(r.body, "startStationCode", from)
	r.body = setJSONField(r.body, "endStationCode", to)
	if u, err := url.Parse(r.url); err == nil {
		q := u.Query()
		override(q, "startStationCode", from)
		override(q, "endStationCode", to)
		override(q, "departureDateFrom", date)
		override(q, "standard_passengers", c.Passengers)
		override(q, "child_passengers", c.Child)
		override(q, "disabled_passengers", c.Disabled)
		if len(q) > 0 {
			u.RawQuery = q.Encode()
			r.url = u.String()
		}
	}
	return r
}

// setJSONField rewrites "key":"..." or "key":123 in a raw JSON body.
func setJSONField(body, key, val string) string {
	if body == "" {
		return body
	}
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*("[^"]*"|\d+)`)
	return re.ReplaceAllString(body, `"`+key+`":"`+val+`"`)
}

func stripComments(s string) string {
	var b strings.Builder
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func override(q url.Values, key, val string) {
	if _, ok := q[key]; ok && val != "" {
		q.Set(key, val)
	}
}

// parseCurl turns a browser "Copy as cURL" string into a request.
func parseCurl(curl string) *request {
	curl = strings.TrimSpace(curl)
	if curl == "" {
		return nil
	}
	curl = strings.ReplaceAll(curl, "\\\n", " ")
	toks := shellSplit(curl)
	if len(toks) > 0 && toks[0] == "curl" {
		toks = toks[1:]
	}
	r := &request{headers: map[string]string{}}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		next := func() string {
			if i+1 < len(toks) {
				i++
				return toks[i]
			}
			return ""
		}
		switch {
		case t == "-X" || t == "--request":
			r.method = next()
		case t == "-H" || t == "--header":
			if k, v, ok := strings.Cut(next(), ":"); ok {
				r.headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		case t == "-d" || t == "--data" || t == "--data-raw" || t == "--data-binary" || t == "--data-urlencode":
			r.body = next()
		case t == "-b" || t == "--cookie":
			r.headers["Cookie"] = next()
		case strings.HasPrefix(t, "http"):
			r.url = t
		case strings.HasPrefix(t, "-"):
			// no-arg flags we ignore (--compressed, -s, -L, -k, ...)
		}
	}
	if r.method == "" {
		if r.body != "" {
			r.method = http.MethodPost
		} else {
			r.method = http.MethodGet
		}
	}
	return r
}

// shellSplit is a minimal tokenizer handling ' and " quoting.
func shellSplit(s string) []string {
	var out []string
	var cur strings.Builder
	inTok := false
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'':
			inTok = true
			i++
			for i < len(s) && s[i] != '\'' {
				cur.WriteByte(s[i])
				i++
			}
			i++
		case c == '"':
			inTok = true
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				cur.WriteByte(s[i])
				i++
			}
			i++
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inTok {
				out = append(out, cur.String())
				cur.Reset()
				inTok = false
			}
			i++
		default:
			inTok = true
			cur.WriteByte(c)
			i++
		}
	}
	if inTok {
		out = append(out, cur.String())
	}
	return out
}

// ───────────────────── response schema (gr.com.ge) ─────────────────────────

type ride struct {
	RideNumber int    `json:"rideNumber"`
	StartDate  string `json:"startDate"`
	// rideStartStation/rideEndStation are the train's full route endpoints (an
	// international train Yerevan→Batumi shows Yerevan there, with Yerevan
	// departure times). startStation/endStation are the SEARCHED segment
	// (e.g. Tbilisi→Batumi) — always prefer those for matching and display.
	RideStartStation rideStation `json:"rideStartStation"`
	RideEndStation   rideStation `json:"rideEndStation"`
	StartStation     rideStation `json:"startStation"`
	EndStation       rideStation `json:"endStation"`
	SeatClasses      []seatAvail `json:"availableSeatsClasses"`
}

type rideStation struct {
	Station struct {
		Code string `json:"code"`
		Name string `json:"name"`
	} `json:"station"`
	DepartureTime       string `json:"departureTime"` // "08:00:00" (local)
	DepartureTimeHour   int    `json:"departureTimeHour"`
	DepartureTimeMinute int    `json:"departureTimeMinute"`
	ArrivalTime         string `json:"arrivalTime"` // "07:07:00" (local)
}

// seg returns the searched-segment stations, falling back to the ride's own
// endpoints when the segment fields are absent (older response shape).
func (r *ride) seg() (dep, arr *rideStation) {
	dep, arr = &r.StartStation, &r.EndStation
	if dep.Station.Code == "" {
		dep = &r.RideStartStation
	}
	if arr.Station.Code == "" {
		arr = &r.RideEndStation
	}
	return dep, arr
}

type seatAvail struct {
	AvailableNumberOfSeats int `json:"availableNumberOfSeats"`
	SeatClass              struct {
		Code string `json:"code"`
		Name string `json:"name"`
	} `json:"seatClass"`
	PriceOfSeats struct {
		Amount       float64 `json:"amount"`
		CurrencyCode string  `json:"currencyCode"`
	} `json:"priceOfSeats"`
}

func (r *ride) hhmm() string {
	dep, _ := r.seg()
	return fmt.Sprintf("%02d:%02d", dep.DepartureTimeHour, dep.DepartureTimeMinute)
}

// arrHHMM is the searched-segment arrival time ("" if unknown).
func (r *ride) arrHHMM() string {
	_, arr := r.seg()
	return normHHMM(arr.ArrivalTime)
}

// parseRides handles both [[...]] and [...] shapes.
func parseRides(body []byte) ([]ride, error) {
	var nested [][]ride
	if err := json.Unmarshal(body, &nested); err == nil {
		var all []ride
		for _, grp := range nested {
			all = append(all, grp...)
		}
		return all, nil
	}
	var flat []ride
	if err := json.Unmarshal(body, &flat); err == nil {
		return flat, nil
	}
	return nil, fmt.Errorf("unexpected response shape")
}

// normHHMM turns "8:0", "08:00", "08:00:00" into "08:00" ("" stays "").
func normHHMM(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	parts := strings.Split(s, ":")
	h, _ := strconv.Atoi(parts[0])
	m := 0
	if len(parts) > 1 {
		m, _ = strconv.Atoi(parts[1])
	}
	return fmt.Sprintf("%02d:%02d", h, m)
}

// findRide returns the ride matching a search's train number and/or time.
func findRide(rides []ride, s *Search) (*ride, bool) {
	wantNum, hasNum := 0, false
	if s.TrainNum != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s.TrainNum)); err == nil {
			wantNum, hasNum = n, true
		}
	}
	wantTime := normHHMM(s.DepTime)
	hasTime := wantTime != ""

	for i := range rides {
		r := &rides[i]
		dep, _ := r.seg()
		numOK := !hasNum || r.RideNumber == wantNum
		timeOK := !hasTime || r.hhmm() == wantTime ||
			strings.HasPrefix(dep.DepartureTime, wantTime)
		if numOK && timeOK {
			return r, true
		}
	}
	return nil, false
}

// seatInfo totals available seats and returns a per-class breakdown.
func seatInfo(r *ride) (total int, lines []string) {
	for _, sc := range r.SeatClasses {
		total += sc.AvailableNumberOfSeats
		nm := sc.SeatClass.Name
		if nm == "" {
			nm = sc.SeatClass.Code
		}
		lines = append(lines, fmt.Sprintf("%s: %d @ %g %s",
			nm, sc.AvailableNumberOfSeats, sc.PriceOfSeats.Amount, sc.PriceOfSeats.CurrencyCode))
	}
	return total, lines
}

func summarizeRides(rides []ride) string {
	parts := make([]string, 0, len(rides))
	for i := range rides {
		r := &rides[i]
		t, _ := seatInfo(r)
		parts = append(parts, fmt.Sprintf("N%d@%s(%d)", r.RideNumber, r.hhmm(), t))
	}
	return strings.Join(parts, ", ")
}

// diagnose explains why findRide returned nothing for a search.
func diagnose(rides []ride, s *Search) string {
	if s.TrainNum != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s.TrainNum)); err == nil {
			for i := range rides {
				if rides[i].RideNumber == n {
					got := rides[i].hhmm()
					if s.DepTime != "" && normHHMM(s.DepTime) != got {
						return fmt.Sprintf("N%d is in the results but departs %s, not %s. "+
							"Cancel this search and re-add it with time %s (or with the train number only).",
							n, got, s.DepTime, got)
					}
					return fmt.Sprintf("N%d found at %s.", n, got)
				}
			}
			return fmt.Sprintf("N%d is not in the results for %s. Trains found: %s", n, s.Date, summarizeRides(rides))
		}
	}
	return "No train matched. Trains found: " + summarizeRides(rides)
}

// ─────────────────────────────── HTTP fetch ────────────────────────────────

func fetch(client *http.Client, r *request) ([]byte, error) {
	var br io.Reader
	if r.body != "" {
		br = strings.NewReader(r.body)
	}
	req, err := http.NewRequest(r.method, r.url, br)
	if err != nil {
		return nil, err
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}
	return body, nil
}

// ──────────────────────────────── watcher ──────────────────────────────────

const maxSearchesPerChat = 10

// Watcher is the GR parser. It implements parser.Parser.
type Watcher struct {
	cfg    *Config
	client *http.Client
	reg    *subs.Registry
	tg     *tg.Telegram
	store  *store

	mu        sync.Mutex // guards checks/lastCheck and serializes checkAll
	checks    int
	lastCheck time.Time
}

// New builds the GR watcher, loading defaults from the environment and active
// searches from the searches file.
func New(reg *subs.Registry, t *tg.Telegram) (*Watcher, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		reg:    reg,
		tg:     t,
		store:  newStore(cfg.SearchesFile),
	}, nil
}

func (w *Watcher) Name() string { return name }

// fetchRides performs one API call for a route+date and parses the trains.
func (w *Watcher) fetchRides(from, to, date string) ([]ride, error) {
	req, err := buildRequest(w.cfg, from, to, date)
	if err != nil {
		return nil, err
	}
	body, err := fetch(w.client, req)
	if err != nil {
		return nil, err
	}
	return parseRides(body)
}

// searchResult carries one search's poll outcome.
type searchResult struct {
	search    Search
	err       error
	notFound  bool
	diag      string
	ride      *ride
	total     int
	breakdown string
	available bool
}

// evaluate matches one search against fetched rides (or a fetch error) and
// updates the search's runtime state.
func (w *Watcher) evaluate(s Search, rides []ride, fetchErr error) searchResult {
	res := searchResult{search: s}
	if fetchErr != nil {
		res.err = fetchErr
		w.store.Update(s.ID, false, func(x *Search) { x.LastResult = "fetch error: " + fetchErr.Error() })
		return res
	}
	r, ok := findRide(rides, &s)
	if !ok {
		res.notFound = true
		res.diag = diagnose(rides, &s)
		w.store.Update(s.ID, false, func(x *Search) { x.LastResult = res.diag; x.Available = false })
		return res
	}
	total, lines := seatInfo(r)
	res.ride, res.total = r, total
	res.breakdown = strings.Join(lines, ", ")
	if res.breakdown == "" {
		res.breakdown = "no classes on sale"
	}
	res.available = total > 0
	w.store.Update(s.ID, false, func(x *Search) {
		x.Available = res.available
		if res.available {
			x.LastResult = fmt.Sprintf("AVAILABLE — %d seats (%s)", total, res.breakdown)
		} else {
			x.LastResult = "no seats (sold out)"
		}
	})
	return res
}

// checkAll expires old searches, fetches each unique route+date once, evaluates
// every search and (if alert=true) notifies owner chats about free seats.
func (w *Watcher) checkAll(alert bool) []searchResult {
	w.mu.Lock()
	defer w.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	for _, s := range w.store.ExpireBefore(today) {
		w.tg.SendTo(s.ChatID, fmt.Sprintf("🗓 Search #%d (%s %s %s) expired — its date has passed.",
			s.ID, routeLabel(s.FromCode, s.ToCode), s.Date, s.targetShort()))
	}

	searches := w.store.Snapshot()
	if len(searches) == 0 {
		return nil
	}

	w.checks++
	w.lastCheck = time.Now()

	// One fetch per unique route+date.
	type fetched struct {
		rides []ride
		err   error
	}
	cache := map[routeKey]fetched{}
	ts := time.Now().Format("15:04:05")
	var results []searchResult
	for i := range searches {
		s := searches[i]
		key := s.routeKey()
		f, ok := cache[key]
		if !ok {
			rides, err := w.fetchRides(key.From, key.To, key.Date)
			f = fetched{rides: rides, err: err}
			cache[key] = f
			if err != nil {
				log.Printf("[gr] [%s] %s %s: fetch error: %v", ts, routeLabel(key.From, key.To), key.Date, err)
			}
		}
		res := w.evaluate(s, f.rides, f.err)
		results = append(results, res)
		w.logResult(ts, res)
		if alert {
			w.maybeAlert(res)
		}
	}
	return results
}

func (w *Watcher) logResult(ts string, res searchResult) {
	s := res.search
	switch {
	case res.err != nil:
		// fetch errors logged once per route in checkAll
	case res.notFound:
		log.Printf("[gr] [%s] #%d %s", ts, s.ID, res.diag)
	case res.available:
		log.Printf("[gr] [%s] #%d N%d @ %s — SEATS AVAILABLE: %d (%s)", ts, s.ID, res.ride.RideNumber, res.ride.hhmm(), res.total, res.breakdown)
	default:
		log.Printf("[gr] [%s] #%d N%d @ %s — no seats (sold out)", ts, s.ID, res.ride.RideNumber, res.ride.hhmm())
	}
}

// maybeAlert notifies the search's chat: free seats (throttled by AlertRepeat)
// or a one-time "train not found" warning. Respects the chat's mute setting.
func (w *Watcher) maybeAlert(res searchResult) {
	s := res.search
	p := w.reg.Get(s.ChatID, name)
	if !p.Subscribed || p.Muted {
		return
	}
	switch {
	case res.err != nil:
		// transient; don't spam
	case res.notFound:
		if !s.Warned {
			w.store.Update(s.ID, true, func(x *Search) { x.Warned = true })
			w.tg.SendTo(s.ChatID, fmt.Sprintf("⚠️ Search #%d: %s", s.ID, res.diag))
		}
	case res.available:
		if s.Warned {
			w.store.Update(s.ID, true, func(x *Search) { x.Warned = false })
		}
		if time.Since(s.LastAlert) > w.cfg.AlertRepeat {
			w.store.Update(s.ID, true, func(x *Search) { x.LastAlert = time.Now() })
			w.tg.SendTo(s.ChatID, w.alertText(res))
		}
	default:
		if s.Warned {
			w.store.Update(s.ID, true, func(x *Search) { x.Warned = false })
		}
	}
}

func (w *Watcher) alertText(res searchResult) string {
	r := res.ride
	s := res.search
	dep, arr := r.seg()
	return fmt.Sprintf("✅ Search #%d — seats available!\nN%d %s→%s at %s on %s\n\n%s\nTotal: %d\n\nBook now: %s",
		s.ID, r.RideNumber, dep.Station.Name, arr.Station.Name,
		r.hhmm(), s.Date, res.breakdown, res.total, w.cfg.bookURL(s.FromCode, s.ToCode, s.Date))
}

// statusFor formats one search's poll result as a reply line.
func (w *Watcher) statusFor(res searchResult) string {
	s := res.search
	head := fmt.Sprintf("#%d %s %s %s", s.ID, routeLabel(s.FromCode, s.ToCode), s.Date, s.targetShort())
	switch {
	case res.err != nil:
		return head + " — ⚠️ check failed: " + res.err.Error()
	case res.notFound:
		return head + " — ⚠️ " + res.diag
	case res.available:
		return head + fmt.Sprintf(" — ✅ %d seats (%s)\nBook: %s",
			res.total, res.breakdown, w.cfg.bookURL(s.FromCode, s.ToCode, s.Date))
	default:
		return head + " — no seats yet (sold out)"
	}
}

// targetShort describes what the search matches on, e.g. "N803 @ 08:00".
func (s *Search) targetShort() string {
	var parts []string
	if s.TrainNum != "" {
		parts = append(parts, "N"+s.TrainNum)
	}
	if s.DepTime != "" {
		parts = append(parts, "@ "+s.DepTime)
	}
	if len(parts) == 0 {
		return "any train"
	}
	return strings.Join(parts, " ")
}

// Run is the independent poll loop plus optional heartbeat.
func (w *Watcher) Run(ctx context.Context) {
	log.Printf("[gr] multi-search watcher | %d active searches | every %s",
		len(w.store.Snapshot()), w.cfg.Interval)

	if w.cfg.HeartbeatEver > 0 {
		go func() {
			t := time.NewTicker(w.cfg.HeartbeatEver)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					w.heartbeat()
				}
			}
		}()
	}

	w.checkAll(true)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[gr] shutting down")
			w.tg.Broadcast(w.reg.Targets(name, true), "👋 GR watcher stopped.")
			return
		case <-ticker.C:
			w.checkAll(true)
		}
	}
}

// heartbeat sends each subscribed chat a summary of its own searches.
func (w *Watcher) heartbeat() {
	w.mu.Lock()
	n, last := w.checks, w.lastCheck
	w.mu.Unlock()
	for _, chatID := range w.reg.Targets(name, false) {
		list := w.store.ForChat(chatID)
		if len(list) == 0 {
			continue
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "⏱ Still watching %d search(es) — %d checks, last at %s:\n",
			len(list), n, last.Format("15:04:05"))
		for i := range list {
			s := &list[i]
			res := s.LastResult
			if res == "" {
				res = "—"
			}
			fmt.Fprintf(&sb, "#%d %s %s %s: %s\n", s.ID, routeLabel(s.FromCode, s.ToCode), s.Date, s.targetShort(), res)
		}
		w.tg.SendTo(chatID, strings.TrimSpace(sb.String()))
	}
}

// ────────────────────────────── commands ───────────────────────────────────

const usage = "Usage:\n" +
	"/gr_trains <tb|bt> <date> — list trains & seats (tb = Tbilisi→Batumi, bt = Batumi→Tbilisi; date = YYYY-MM-DD, today, tomorrow)\n" +
	"/gr_search <tb|bt> <date> <HH:MM and/or train number> — start watching a train, e.g.\n" +
	"   /gr_search tb 2026-08-20 06:00\n" +
	"   /gr_search bt tomorrow 803\n" +
	"/gr_list — your active searches\n" +
	"/gr_cancel <id|all> — stop a search\n" +
	"/gr_check [id] — check now"

// parseDate accepts YYYY-MM-DD, "today", "tomorrow"; rejects past dates.
func parseDate(tok string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(tok))
	now := time.Now()
	switch t {
	case "today":
		return now.Format("2006-01-02"), nil
	case "tomorrow":
		return now.AddDate(0, 0, 1).Format("2006-01-02"), nil
	}
	d, err := time.Parse("2006-01-02", t)
	if err != nil {
		return "", fmt.Errorf("bad date %q — use YYYY-MM-DD, today or tomorrow", tok)
	}
	if d.Format("2006-01-02") < now.Format("2006-01-02") {
		return "", fmt.Errorf("date %s is in the past", tok)
	}
	return d.Format("2006-01-02"), nil
}

var timeTokRe = regexp.MustCompile(`^\d{1,2}:\d{2}$`)
var numTokRe = regexp.MustCompile(`^n?\d{1,5}$`)

// parseTarget classifies trailing tokens into departure time and train number.
func parseTarget(toks []string) (depTime, trainNum string, err error) {
	for _, t := range toks {
		lt := strings.ToLower(strings.TrimSpace(t))
		switch {
		case timeTokRe.MatchString(lt):
			depTime = normHHMM(lt)
		case numTokRe.MatchString(lt):
			trainNum = strings.TrimPrefix(lt, "n")
		default:
			return "", "", fmt.Errorf("can't understand %q — expected HH:MM or a train number", t)
		}
	}
	if depTime == "" && trainNum == "" {
		return "", "", fmt.Errorf("give a departure time (HH:MM) and/or a train number")
	}
	return depTime, trainNum, nil
}

// Handle processes /gr_<action> commands for one chat.
func (w *Watcher) Handle(chatID, action, args string) string {
	switch action {
	case "start":
		w.reg.Subscribe(chatID, name)
		return "🚆 Subscribed to GR (Georgian Railway) alerts.\n\n" + usage
	case "stop":
		w.reg.Mute(chatID, name, true)
		return "🔕 GR seat alerts paused for this chat (searches keep running). /gr_continue to resume."
	case "continue":
		w.reg.Mute(chatID, name, false)
		return "🔔 GR seat alerts resumed."
	case "heartbeat_off":
		w.reg.SetHeartbeat(chatID, name, true)
		return "⏱🚫 GR heartbeat off for this chat. (/gr_heartbeat_on to re-enable)"
	case "heartbeat_on":
		w.reg.SetHeartbeat(chatID, name, false)
		return "⏱ GR heartbeat on for this chat."
	case "trains":
		return w.handleTrains(args)
	case "search", "add":
		return w.handleSearch(chatID, args)
	case "list":
		return w.handleList(chatID)
	case "cancel", "del", "delete":
		return w.handleCancel(chatID, args)
	case "check":
		return w.handleCheck(chatID, args)
	case "status":
		return w.StatusLine(chatID)
	default:
		return ""
	}
}

// handleTrains lists the trains the API returns for a direction+date.
func (w *Watcher) handleTrains(args string) string {
	toks := strings.Fields(args)
	if len(toks) < 2 {
		return "⚠️ " + usage
	}
	from, to, ok := parseDirection(toks[0])
	if !ok {
		return "⚠️ Bad direction — use tb (Tbilisi→Batumi) or bt (Batumi→Tbilisi)."
	}
	date, err := parseDate(toks[1])
	if err != nil {
		return "⚠️ " + err.Error()
	}
	rides, err := w.fetchRides(from, to, date)
	if err != nil {
		return "⚠️ fetch failed: " + err.Error()
	}
	if len(rides) == 0 {
		return fmt.Sprintf("No trains returned for %s on %s.", routeLabel(from, to), date)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🚆 %s on %s:\n", routeLabel(from, to), date)
	for i := range rides {
		r := &rides[i]
		total, lines := seatInfo(r)
		seats := "sold out"
		if total > 0 {
			seats = fmt.Sprintf("%d seats (%s)", total, strings.Join(lines, ", "))
		}
		arrive := ""
		if a := r.arrHHMM(); a != "" {
			arrive = " → " + a
		}
		fmt.Fprintf(&sb, "N%d @ %s%s — %s\n", r.RideNumber, r.hhmm(), arrive, seats)
	}
	sb.WriteString("\nStart watching one: /gr_search " + dirToken(from) + " " + date + " <HH:MM or train number>")
	return sb.String()
}

func dirToken(from string) string {
	if from == tbilisiCode {
		return "tb"
	}
	return "bt"
}

// handleSearch creates a new search for this chat and checks it immediately.
func (w *Watcher) handleSearch(chatID, args string) string {
	toks := strings.Fields(args)
	if len(toks) < 3 {
		return "⚠️ " + usage
	}
	from, to, ok := parseDirection(toks[0])
	if !ok {
		return "⚠️ Bad direction — use tb (Tbilisi→Batumi) or bt (Batumi→Tbilisi)."
	}
	date, err := parseDate(toks[1])
	if err != nil {
		return "⚠️ " + err.Error()
	}
	depTime, trainNum, err := parseTarget(toks[2:])
	if err != nil {
		return "⚠️ " + err.Error()
	}

	// Refuse duplicates of an identical live search for this chat.
	for _, ex := range w.store.ForChat(chatID) {
		if ex.FromCode == from && ex.Date == date && ex.DepTime == depTime && ex.TrainNum == trainNum {
			return fmt.Sprintf("You already watch this train — search #%d.", ex.ID)
		}
	}
	if w.store.CountForChat(chatID) >= maxSearchesPerChat {
		return fmt.Sprintf("⚠️ Limit of %d active searches per chat reached. /gr_cancel one first.", maxSearchesPerChat)
	}

	w.reg.Subscribe(chatID, name)
	s := w.store.Add(Search{
		ChatID:   chatID,
		FromCode: from,
		ToCode:   to,
		Date:     date,
		DepTime:  depTime,
		TrainNum: trainNum,
	})

	// Immediate first check so the user sees the current state.
	rides, ferr := w.fetchRides(from, to, date)
	res := w.evaluate(s, rides, ferr)
	return fmt.Sprintf("🔍 Search #%d started: %s %s %s, checking every %s.\n\nRight now: %s",
		s.ID, routeLabel(from, to), date, s.targetShort(), w.cfg.Interval, w.statusFor(res))
}

func (w *Watcher) handleList(chatID string) string {
	list := w.store.ForChat(chatID)
	if len(list) == 0 {
		return "No active searches. Start one:\n\n" + usage
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Your active searches (%d):\n", len(list))
	for i := range list {
		s := &list[i]
		res := s.LastResult
		if res == "" {
			res = "not checked yet"
		}
		fmt.Fprintf(&sb, "#%d %s %s %s — %s\n", s.ID, routeLabel(s.FromCode, s.ToCode), s.Date, s.targetShort(), res)
	}
	sb.WriteString("\n/gr_cancel <id> to stop one, /gr_check <id> to check now")
	return sb.String()
}

func (w *Watcher) handleCancel(chatID, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	if arg == "" {
		return "⚠️ Usage: /gr_cancel <id|all> — see ids in /gr_list"
	}
	if arg == "all" {
		n := w.store.RemoveAll(chatID)
		if n == 0 {
			return "No active searches to cancel."
		}
		return fmt.Sprintf("🗑 Cancelled %d search(es).", n)
	}
	id, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	if err != nil {
		return "⚠️ Bad id — use a number from /gr_list, or 'all'."
	}
	s, ok := w.store.Remove(chatID, id)
	if !ok {
		return fmt.Sprintf("Search #%d not found among your searches. /gr_list to see them.", id)
	}
	return fmt.Sprintf("🗑 Search #%d (%s %s %s) cancelled.", s.ID, routeLabel(s.FromCode, s.ToCode), s.Date, s.targetShort())
}

// handleCheck polls now: one search by id, or all of this chat's searches.
func (w *Watcher) handleCheck(chatID, args string) string {
	arg := strings.TrimSpace(args)
	if arg != "" {
		id, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil {
			return "⚠️ Bad id — use a number from /gr_list."
		}
		for _, s := range w.store.ForChat(chatID) {
			if s.ID == id {
				rides, ferr := w.fetchRides(s.FromCode, s.ToCode, s.Date)
				return "Checked now:\n" + w.statusFor(w.evaluate(s, rides, ferr))
			}
		}
		return fmt.Sprintf("Search #%d not found among your searches.", id)
	}

	list := w.store.ForChat(chatID)
	if len(list) == 0 {
		return "No active searches to check. Start one:\n\n" + usage
	}
	results := w.checkAll(false)
	var sb strings.Builder
	sb.WriteString("Checked now:\n")
	for _, res := range results {
		if res.search.ChatID != chatID {
			continue
		}
		sb.WriteString(w.statusFor(res) + "\n")
	}
	return strings.TrimSpace(sb.String())
}

// StatusLine reports the GR parser state for a chat.
func (w *Watcher) StatusLine(chatID string) string {
	w.mu.Lock()
	n, last := w.checks, w.lastCheck
	w.mu.Unlock()
	when := "never"
	if !last.IsZero() {
		when = last.Format("15:04:05") + fmt.Sprintf(" (%s ago)", time.Since(last).Round(time.Second))
	}
	p := w.reg.Get(chatID, name)
	list := w.store.ForChat(chatID)
	var sb strings.Builder
	fmt.Fprintf(&sb, "🚆 GR — %d active search(es), poll every %s\nChecks: %d\nLast: %s\n",
		len(list), w.cfg.Interval, n, when)
	for i := range list {
		s := &list[i]
		res := s.LastResult
		if res == "" {
			res = "not checked yet"
		}
		fmt.Fprintf(&sb, "#%d %s %s %s — %s\n", s.ID, routeLabel(s.FromCode, s.ToCode), s.Date, s.targetShort(), res)
	}
	fmt.Fprintf(&sb, "Your subscription: %s | alerts: %s | heartbeat: %s",
		onoff(p.Subscribed), onoff(p.Subscribed && !p.Muted), onoff(p.Subscribed && !p.HeartbeatOff))
	return sb.String()
}

func onoff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
