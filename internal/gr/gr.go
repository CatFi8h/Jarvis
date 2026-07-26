// Package gr is the Georgian Railway ticket parser: it polls gr.com.ge for a free
// seat on a specific train (date + time and/or train number) and alerts
// subscribed chats the moment one appears.
//
// The search target is configured in .env — see .env.example. Optionally a
// captured "Copy as cURL" request (API_CURL_FILE, default request.curl) supplies
// auth cookies/headers; otherwise the known default endpoint is used.
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

// ─────────────────────────────── config ────────────────────────────────────

type Config struct {
	StartStation string
	EndStation   string
	Date         string // YYYY-MM-DD
	DepTime      string // HH:MM (optional)
	TrainNumber  string // e.g. 803 (optional)
	Passengers   string
	Child        string
	Disabled     string

	Interval      time.Duration
	AlertRepeat   time.Duration
	HeartbeatEver time.Duration

	APIEndpoint string
	RouteType   int

	CurlFile   string
	APIURL     string
	APIMethod  string
	APIBody    string
	APIHeaders map[string]string
}

func loadConfig() (*Config, error) {
	c := &Config{
		StartStation: env("START_STATION_CODE", "57151"),
		EndStation:   env("END_STATION_CODE", "56014"),
		Date:         env("DEPARTURE_DATE", ""),
		DepTime:      strings.TrimSpace(env("DEPARTURE_TIME", "")),
		TrainNumber:  strings.TrimSpace(env("TRAIN_NUMBER", "")),
		Passengers:   env("PASSENGERS", "1"),
		Child:        env("CHILD_PASSENGERS", "0"),
		Disabled:     env("DISABLED_PASSENGERS", "0"),
		APIEndpoint:  env("API_ENDPOINT", defaultEndpoint),
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

	if c.Date == "" {
		return nil, fmt.Errorf("DEPARTURE_DATE is required (YYYY-MM-DD)")
	}
	if c.DepTime == "" && c.TrainNumber == "" {
		return nil, fmt.Errorf("set DEPARTURE_TIME and/or TRAIN_NUMBER so the bot knows which train")
	}
	if c.Interval < time.Minute {
		c.Interval = time.Minute // be polite to the site
	}
	return c, nil
}

func (c *Config) bookURL() string {
	return fmt.Sprintf("https://gr.com.ge/en/search?startStationCode=%s&endStationCode=%s"+
		"&departureDateFrom=%s&standard_passengers=%s&child_passengers=%s&disabled_passengers=%s",
		c.StartStation, c.EndStation, c.Date, c.Passengers, c.Child, c.Disabled)
}

func (c *Config) target() string {
	parts := []string{}
	if c.TrainNumber != "" {
		parts = append(parts, "N"+c.TrainNumber)
	}
	if c.DepTime != "" {
		parts = append(parts, c.DepTime)
	}
	return strings.Join(parts, " @ ") + " on " + c.Date
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

// buildRequest constructs the poll request. Priority: (1) captured cURL in
// CurlFile, (2) explicit API_URL + API_BODY, (3) the known default POST.
func buildRequest(c *Config) (*request, error) {
	if raw, err := os.ReadFile(c.CurlFile); err == nil {
		if curl := stripComments(string(raw)); strings.Contains(curl, "http") {
			r := parseCurl(curl)
			return applyOverrides(c, r), nil
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
		return applyOverrides(c, r), nil
	}

	body, err := json.Marshal(searchBody{
		StartStationCode:   c.StartStation,
		EndStationCode:     c.EndStation,
		DepartureDateFrom:  c.Date,
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

// applyOverrides forces the target date and query params to match config.
func applyOverrides(c *Config, r *request) *request {
	r.url = dateRe.ReplaceAllString(r.url, c.Date)
	r.body = dateRe.ReplaceAllString(r.body, c.Date)
	if u, err := url.Parse(r.url); err == nil {
		q := u.Query()
		override(q, "startStationCode", c.StartStation)
		override(q, "endStationCode", c.EndStation)
		override(q, "departureDateFrom", c.Date)
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
	RideNumber       int         `json:"rideNumber"`
	StartDate        string      `json:"startDate"`
	RideStartStation rideStation `json:"rideStartStation"`
	RideEndStation   rideStation `json:"rideEndStation"`
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
	return fmt.Sprintf("%02d:%02d", r.RideStartStation.DepartureTimeHour, r.RideStartStation.DepartureTimeMinute)
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

// findRide returns the ride matching train number and/or departure time.
func findRide(rides []ride, c *Config) (*ride, bool) {
	wantNum, hasNum := 0, false
	if c.TrainNumber != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(c.TrainNumber)); err == nil {
			wantNum, hasNum = n, true
		}
	}
	wantTime := normHHMM(c.DepTime)
	hasTime := wantTime != ""

	for i := range rides {
		r := &rides[i]
		numOK := !hasNum || r.RideNumber == wantNum
		timeOK := !hasTime || r.hhmm() == wantTime ||
			strings.HasPrefix(r.RideStartStation.DepartureTime, wantTime)
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

// diagnose explains why findRide returned nothing.
func diagnose(rides []ride, c *Config) string {
	if c.TrainNumber != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(c.TrainNumber)); err == nil {
			for i := range rides {
				if rides[i].RideNumber == n {
					got := rides[i].hhmm()
					if c.DepTime != "" && normHHMM(c.DepTime) != got {
						return fmt.Sprintf("N%d is in the results but departs %s, not your DEPARTURE_TIME=%s. "+
							"Set DEPARTURE_TIME=%s in .env (or clear it to match by number only).",
							n, got, c.DepTime, got)
					}
					return fmt.Sprintf("N%d found at %s.", n, got)
				}
			}
			return fmt.Sprintf("N%d is not in the results for %s. Trains found: %s", n, c.Date, summarizeRides(rides))
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

// ──────────────────────────── watcher / state ──────────────────────────────

type state struct {
	mu         sync.Mutex
	checks     int
	lastCheck  time.Time
	lastResult string
	available  bool
	lastAlert  time.Time
}

// checkResult carries one poll's outcome for broadcasting and /check replies.
type checkResult struct {
	err       error
	notFound  bool
	diag      string
	ride      *ride
	total     int
	breakdown string
	available bool
}

// Watcher is the GR parser. It implements parser.Parser.
type Watcher struct {
	cfg    *Config
	req    *request
	client *http.Client
	reg    *subs.Registry
	tg     *tg.Telegram
	state  *state

	pollMu         sync.Mutex // serializes poll(), guards firstRun/warnedNotFound
	firstRun       bool
	warnedNotFound bool
}

// New builds the GR watcher, loading config and the poll request from the
// environment. Returns an error if required config is missing.
func New(reg *subs.Registry, t *tg.Telegram) (*Watcher, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	req, err := buildRequest(cfg)
	if err != nil {
		return nil, err
	}
	return &Watcher{
		cfg:      cfg,
		req:      req,
		client:   &http.Client{Timeout: 30 * time.Second},
		reg:      reg,
		tg:       t,
		state:    &state{},
		firstRun: true,
	}, nil
}

func (w *Watcher) Name() string { return name }

// poll performs one fetch+parse+match, updates state, and returns the result.
func (w *Watcher) poll() checkResult {
	w.pollMu.Lock()
	defer w.pollMu.Unlock()

	var res checkResult
	body, err := fetch(w.client, w.req)
	w.state.mu.Lock()
	w.state.checks++
	w.state.lastCheck = time.Now()
	w.state.mu.Unlock()

	ts := time.Now().Format("15:04:05")
	if err != nil {
		res.err = err
		log.Printf("[gr] [%s] fetch error: %v", ts, err)
		w.state.mu.Lock()
		w.state.lastResult = "fetch error: " + err.Error()
		w.state.mu.Unlock()
		return res
	}

	rides, perr := parseRides(body)
	if perr != nil {
		res.err = perr
		log.Printf("[gr] [%s] parse error: %v", ts, perr)
		if w.firstRun {
			snippet := string(body)
			if len(snippet) > 500 {
				snippet = snippet[:500]
			}
			log.Println("[gr] raw response head:", snippet)
		}
		w.state.mu.Lock()
		w.state.lastResult = "parse error: " + perr.Error()
		w.state.mu.Unlock()
		w.firstRun = false
		return res
	}

	if w.firstRun {
		log.Printf("[gr] first check — trains returned: %s", summarizeRides(rides))
	}
	w.firstRun = false

	r, ok := findRide(rides, w.cfg)
	if !ok {
		res.notFound = true
		res.diag = diagnose(rides, w.cfg)
		log.Printf("[gr] [%s] %s", ts, res.diag)
		w.state.mu.Lock()
		w.state.lastResult = res.diag
		w.state.available = false
		w.state.mu.Unlock()
		return res
	}

	total, lines := seatInfo(r)
	res.ride, res.total = r, total
	res.breakdown = strings.Join(lines, ", ")
	if res.breakdown == "" {
		res.breakdown = "no classes on sale"
	}
	res.available = total > 0

	w.state.mu.Lock()
	w.state.available = res.available
	if res.available {
		w.state.lastResult = fmt.Sprintf("AVAILABLE — %d seats (%s)", total, res.breakdown)
	} else {
		w.state.lastResult = "no seats (sold out)"
	}
	w.state.mu.Unlock()

	if res.available {
		log.Printf("[gr] [%s] N%d @ %s — SEATS AVAILABLE: %d (%s)", ts, r.RideNumber, r.hhmm(), total, res.breakdown)
	} else {
		log.Printf("[gr] [%s] N%d @ %s — no seats (sold out)", ts, r.RideNumber, r.hhmm())
	}
	return res
}

func (w *Watcher) alertText(res checkResult) string {
	r := res.ride
	return fmt.Sprintf("✅ Seats available — N%d %s→%s at %s on %s\n\n%s\nTotal: %d\n\nBook now: %s",
		r.RideNumber, r.RideStartStation.Station.Name, r.RideEndStation.Station.Name,
		r.hhmm(), w.cfg.Date, res.breakdown, res.total, w.cfg.bookURL())
}

// statusFor formats a poll result as a reply to a /gr_check request.
func (w *Watcher) statusFor(res checkResult) string {
	switch {
	case res.err != nil:
		return "⚠️ check failed: " + res.err.Error()
	case res.notFound:
		return "⚠️ " + res.diag
	case res.available:
		return w.alertText(res)
	default:
		return fmt.Sprintf("N%d @ %s on %s — no seats yet (sold out).",
			res.ride.RideNumber, res.ride.hhmm(), w.cfg.Date)
	}
}

// doCheck runs a poll and broadcasts alerts to subscribers (throttled).
// Only called from the Run loop (single goroutine), so warnedNotFound is safe.
func (w *Watcher) doCheck() {
	res := w.poll()
	switch {
	case res.err != nil:
		// logged in poll
	case res.notFound:
		if !w.warnedNotFound {
			w.warnedNotFound = true
			w.tg.Broadcast(w.reg.Targets(name, true), "⚠️ "+res.diag)
		}
	default:
		w.warnedNotFound = false
		if res.available {
			w.state.mu.Lock()
			should := time.Since(w.state.lastAlert) > w.cfg.AlertRepeat
			if should {
				w.state.lastAlert = time.Now()
			}
			w.state.mu.Unlock()
			if should {
				w.tg.Broadcast(w.reg.Targets(name, true), w.alertText(res))
			}
		}
	}
}

// Run is the independent poll loop plus optional heartbeat.
func (w *Watcher) Run(ctx context.Context) {
	base, _, _ := strings.Cut(w.req.url, "?")
	log.Printf("[gr] watching %s | endpoint %s %s | every %s",
		w.cfg.target(), w.req.method, base, w.cfg.Interval)

	if w.cfg.HeartbeatEver > 0 {
		go func() {
			t := time.NewTicker(w.cfg.HeartbeatEver)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					w.state.mu.Lock()
					n, res := w.state.checks, w.state.lastResult
					w.state.mu.Unlock()
					w.tg.Broadcast(w.reg.Targets(name, false),
						fmt.Sprintf("⏱ Still watching %s — %d checks, latest: %s", w.cfg.target(), n, res))
				}
			}
		}()
	}

	w.doCheck()
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[gr] shutting down")
			w.tg.Broadcast(w.reg.Targets(name, true), "👋 GR watcher stopped.")
			return
		case <-ticker.C:
			w.doCheck()
		}
	}
}

// Handle processes /gr_<action> commands for one chat.
func (w *Watcher) Handle(chatID, action, args string) string {
	switch action {
	case "start":
		w.reg.Subscribe(chatID, name)
		return fmt.Sprintf("🚆 Subscribed to GR. Watching %s every %s. I'll ping you when a seat opens.",
			w.cfg.target(), w.cfg.Interval)
	case "stop":
		w.reg.Mute(chatID, name, true)
		return "🔕 GR seat alerts paused for this chat. Send /gr_continue to resume."
	case "continue":
		w.reg.Mute(chatID, name, false)
		return "🔔 GR seat alerts resumed."
	case "heartbeat_off":
		w.reg.SetHeartbeat(chatID, name, true)
		return "⏱🚫 GR heartbeat off for this chat. (/gr_heartbeat_on to re-enable)"
	case "heartbeat_on":
		w.reg.SetHeartbeat(chatID, name, false)
		return "⏱ GR heartbeat on for this chat."
	case "status":
		return w.StatusLine(chatID)
	case "check":
		return "Checking GR now…\n" + w.statusFor(w.poll())
	default:
		return ""
	}
}

// StatusLine reports the GR parser state for a chat.
func (w *Watcher) StatusLine(chatID string) string {
	w.state.mu.Lock()
	n, last, res, avail := w.state.checks, w.state.lastCheck, w.state.lastResult, w.state.available
	w.state.mu.Unlock()
	when := "never"
	if !last.IsZero() {
		when = last.Format("15:04:05") + fmt.Sprintf(" (%s ago)", time.Since(last).Round(time.Second))
	}
	if res == "" {
		res = "—"
	}
	p := w.reg.Get(chatID, name)
	return fmt.Sprintf("🚆 GR — %s\nChecks: %d\nLast: %s\nResult: %s\nAvailable now: %v\nYour subscription: %s | alerts: %s | heartbeat: %s",
		w.cfg.target(), n, when, res, avail,
		onoff(p.Subscribed), onoff(p.Subscribed && !p.Muted), onoff(p.Subscribed && !p.HeartbeatOff))
}

func onoff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
