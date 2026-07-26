// gr-ticket-bot — a Telegram bot that watches gr.com.ge for a free seat on a
// specific train (date + time) and messages you the moment one appears.
//
// Everything is configured in .env — see .env.example. Stdlib only, no deps.
//
// Quick start:
//  1. cp .env.example .env   and fill it in
//  2. Capture the site's search request once and save it to request.curl
//     (DevTools -> Network -> Fetch/XHR -> the JSON request -> Copy as cURL).
//  3. go run .        (or: go build -o gr-ticket-bot . && ./gr-ticket-bot)
//
// The bot never buys anything — it only tells you when to.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ─────────────────────────────── config ────────────────────────────────────

type Config struct {
	BotToken        string
	ChatID          string
	SubscribersFile string

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

	APIEndpoint string // default POST endpoint (known)
	RouteType   int

	CurlFile   string
	APIURL     string
	APIMethod  string
	APIBody    string
	APIHeaders map[string]string
}

const defaultEndpoint = "https://gr.com.ge/api/ticket-search"

func loadConfig() (*Config, error) {
	c := &Config{
		BotToken:        env("TELEGRAM_BOT_TOKEN", ""),
		ChatID:          env("TELEGRAM_CHAT_ID", ""),
		SubscribersFile: env("SUBSCRIBERS_FILE", "subscribers.json"),
		StartStation:    env("START_STATION_CODE", "57151"),
		EndStation:      env("END_STATION_CODE", "56014"),
		Date:            env("DEPARTURE_DATE", ""),
		DepTime:         strings.TrimSpace(env("DEPARTURE_TIME", "")),
		TrainNumber:     strings.TrimSpace(env("TRAIN_NUMBER", "")),
		Passengers:      env("PASSENGERS", "1"),
		Child:           env("CHILD_PASSENGERS", "0"),
		Disabled:        env("DISABLED_PASSENGERS", "0"),
		APIEndpoint:     env("API_ENDPOINT", defaultEndpoint),
		CurlFile:        env("API_CURL_FILE", "request.curl"),
		APIURL:          env("API_URL", ""),
		APIMethod:       env("API_METHOD", ""),
		APIBody:         env("API_BODY", ""),
		APIHeaders:      map[string]string{},
	}
	c.RouteType = atoiDefault(env("ROUTE_TYPE", "0"), 0)
	c.Interval = envDur("CHECK_INTERVAL", 5*time.Minute)
	c.AlertRepeat = envDur("ALERT_REPEAT", 30*time.Minute)
	c.HeartbeatEver = envDur("HEARTBEAT_EVERY", 0) // 0 = off
	if v := env("API_HEADERS_JSON", ""); v != "" {
		_ = json.Unmarshal([]byte(v), &c.APIHeaders)
	}

	if c.BotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
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

// ─────────────────────────── .env file loader ──────────────────────────────

// loadDotEnv reads KEY=VALUE lines from path into the process env (without
// overwriting variables already set in the real environment).
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no .env is fine; rely on real env vars
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // long API_HEADERS_JSON lines
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
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
	// allow a bare number of minutes, e.g. "5"
	if n, err := strconv.Atoi(v); err == nil {
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

// searchBody is the JSON payload gr.com.ge's /api/ticket-search expects.
type searchBody struct {
	StartStationCode   string `json:"startStationCode"`
	EndStationCode     string `json:"endStationCode"`
	DepartureDateFrom  string `json:"departureDateFrom"`
	StandardPassengers int    `json:"standard_passengers"`
	ChildPassengers    int    `json:"child_passengers"`
	DisabledPassengers int    `json:"disabled_passengers"`
	RouteType          int    `json:"routeType"`
}

// buildRequest constructs the poll request. Priority:
//  1. an explicit "Copy as cURL" in API_CURL_FILE (use when the endpoint needs
//     auth cookies/headers) — overridden with the target date/params;
//  2. an explicit API_URL + API_BODY;
//  3. the known default: POST https://gr.com.ge/api/ticket-search with a JSON
//     body built from your .env values. No capture needed.
func buildRequest(c *Config) (*request, error) {
	// 1. explicit cURL template (ignoring the commented placeholder file)
	if raw, err := os.ReadFile(c.CurlFile); err == nil {
		if curl := stripComments(string(raw)); strings.Contains(curl, "http") {
			r := parseCurl(curl)
			return applyOverrides(c, r), nil
		}
	}

	// 2. explicit API_URL override
	if c.APIURL != "" {
		method := c.APIMethod
		if method == "" {
			if c.APIBody != "" {
				method = http.MethodPost
			} else {
				method = http.MethodGet
			}
		}
		r := &request{method: method, url: c.APIURL, headers: defaultHeaders(c), body: c.APIBody}
		for k, v := range c.APIHeaders {
			r.headers[k] = v
		}
		return applyOverrides(c, r), nil
	}

	// 3. known default: POST ticket-search with a JSON body from config
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
	r := &request{
		method:  http.MethodPost,
		url:     c.APIEndpoint,
		headers: defaultHeaders(c),
		body:    string(body),
	}
	return r, nil
}

func defaultHeaders(c *Config) map[string]string {
	return map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"User-Agent":   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
		"Origin":       "https://gr.com.ge",
		"Referer":      "https://gr.com.ge/en/search",
	}
}

// applyOverrides forces the target date (and any query-string params the request
// already carries) to match config, so one captured request works for any date.
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

// stripComments removes blank and #-prefixed lines (so the placeholder
// request.curl, which is all comments, is treated as empty).
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
//
// /api/ticket-search returns a JSON array-of-arrays of rides:
//   [[ {ride}, {ride}, ... ]]
// Each ride carries the train number in rideNumber, the origin departure time
// in rideStartStation.departureTime* (LOCAL time), and per-class availability in
// availableSeatsClasses[].availableNumberOfSeats. Empty classes = sold out.

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

// hhmm returns the origin departure time as "HH:MM" (local).
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
// If both are set in config, both must match (most precise).
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
		name := sc.SeatClass.Name
		if name == "" {
			name = sc.SeatClass.Code
		}
		lines = append(lines, fmt.Sprintf("%s: %d @ %g %s",
			name, sc.AvailableNumberOfSeats, sc.PriceOfSeats.Amount, sc.PriceOfSeats.CurrencyCode))
	}
	return total, lines
}

// summarize lists what the search returned (for logs / "not found" diagnostics).
func summarize(rides []ride) string {
	parts := make([]string, 0, len(rides))
	for i := range rides {
		r := &rides[i]
		t, _ := seatInfo(r)
		parts = append(parts, fmt.Sprintf("N%d@%s(%d)", r.RideNumber, r.hhmm(), t))
	}
	return strings.Join(parts, ", ")
}

// diagnose explains why findRide returned nothing, so a config mistake (e.g. the
// wrong DEPARTURE_TIME) is obvious instead of silent.
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
			return fmt.Sprintf("N%d is not in the results for %s. Trains found: %s", n, c.Date, summarize(rides))
		}
	}
	return "No train matched. Trains found: " + summarize(rides)
}

// ─────────────────────────── subscribers ───────────────────────────────────

// Telegram bots can't enumerate the chats they belong to — a chat only becomes
// known when it messages the bot. So we remember every chat that talks to us
// and broadcast to them. Per-chat flags let each chat independently mute seat
// alerts (/stop) or the heartbeat, persisted so they survive restarts.
type subscriber struct {
	ChatID       string `json:"chat_id"`
	Muted        bool   `json:"muted"`         // seat alerts off (/stop)
	HeartbeatOff bool   `json:"heartbeat_off"` // heartbeat off
}

type registry struct {
	mu   sync.Mutex
	path string
	subs map[string]*subscriber
}

func newRegistry(path string) *registry {
	r := &registry{path: path, subs: map[string]*subscriber{}}
	if data, err := os.ReadFile(path); err == nil {
		var list []*subscriber
		if json.Unmarshal(data, &list) == nil {
			for _, s := range list {
				r.subs[s.ChatID] = s
			}
		}
	}
	return r
}

func (r *registry) save() { // caller holds r.mu
	list := make([]*subscriber, 0, len(r.subs))
	for _, s := range r.subs {
		list = append(list, s)
	}
	if data, err := json.MarshalIndent(list, "", "  "); err == nil {
		os.WriteFile(r.path, data, 0o644)
	}
}

// update creates the chat if new (subscribed, unmuted) and applies fn.
func (r *registry) update(chatID string, fn func(*subscriber)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.subs[chatID]
	if !ok {
		s = &subscriber{ChatID: chatID}
		r.subs[chatID] = s
	}
	if fn != nil {
		fn(s)
	}
	r.save()
}

func (r *registry) get(chatID string) subscriber {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.subs[chatID]; ok {
		return *s
	}
	return subscriber{ChatID: chatID}
}

func (r *registry) targets(wantAlert bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for id, s := range r.subs {
		if wantAlert && !s.Muted {
			out = append(out, id)
		}
		if !wantAlert && !s.HeartbeatOff {
			out = append(out, id)
		}
	}
	return out
}

func (r *registry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs)
}

func onoff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// ─────────────────────────── Telegram client ───────────────────────────────

type Telegram struct {
	token string
	http  *http.Client
}

func (t *Telegram) sendTo(chatID, text string) {
	body, _ := json.Marshal(map[string]any{
		"chat_id": chatID, "text": text, "disable_web_page_preview": false,
	})
	resp, err := t.http.Post("https://api.telegram.org/bot"+t.token+"/sendMessage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Println("[tg] send error:", err)
		return
	}
	resp.Body.Close()
}

func (t *Telegram) broadcast(chatIDs []string, text string) {
	for _, id := range chatIDs {
		t.sendTo(id, text)
	}
}

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

// listen long-polls for commands from ANY chat, dispatching each with its chat id.
func (t *Telegram) listen(ctx context.Context, onCommand func(chatID, cmd string)) {
	var offset int64
	client := &http.Client{Timeout: 40 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		u := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?timeout=30&offset=%d", t.token, offset)
		resp, err := client.Get(u)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		var payload struct {
			OK     bool       `json:"ok"`
			Result []tgUpdate `json:"result"`
		}
		json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		for _, up := range payload.Result {
			offset = up.UpdateID + 1
			if up.Message.Chat.ID == 0 || up.Message.Text == "" {
				continue
			}
			chatID := strconv.FormatInt(up.Message.Chat.ID, 10)
			cmd := strings.ToLower(strings.TrimSpace(up.Message.Text))
			if i := strings.IndexAny(cmd, " @"); i > 0 {
				cmd = cmd[:i]
			}
			onCommand(chatID, cmd)
		}
	}
}

// ──────────────────────────── monitor state ────────────────────────────────

type State struct {
	mu         sync.Mutex
	checks     int
	lastCheck  time.Time
	lastResult string
	available  bool
	lastAlert  time.Time
}

func (s *State) snapshot() (int, time.Time, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks, s.lastCheck, s.lastResult, s.available
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

// ─────────────────────────────── main ──────────────────────────────────────

func main() {
	loadDotEnv(".env")
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("[config error]", err)
		os.Exit(1)
	}
	req, err := buildRequest(cfg)
	if err != nil {
		fmt.Println("[config error]", err)
		os.Exit(1)
	}

	reg := newRegistry(cfg.SubscribersFile)
	if cfg.ChatID != "" { // optional bootstrap subscriber from .env
		reg.update(cfg.ChatID, nil)
	}
	tg := &Telegram{token: cfg.BotToken, http: &http.Client{Timeout: 20 * time.Second}}
	client := &http.Client{Timeout: 30 * time.Second}
	state := &State{}
	firstRun := true
	warnedNotFound := false

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	base, _, _ := strings.Cut(req.url, "?")
	fmt.Printf("[i] watching %s | endpoint %s %s | every %s | %d subscriber(s)\n",
		cfg.target(), req.method, base, cfg.Interval, reg.count())

	helpText := "Commands:\n" +
		"/status – current state\n" +
		"/check – check right now\n" +
		"/stop – pause seat alerts for this chat\n" +
		"/continue – resume seat alerts\n" +
		"/heartbeat_off – silence periodic pings\n" +
		"/heartbeat_on – re-enable periodic pings"

	if t := reg.targets(true); len(t) == 0 {
		fmt.Println("[i] No subscribers yet — open the bot in Telegram and send /start to receive alerts.")
	} else {
		tg.broadcast(t, fmt.Sprintf("🚆 Watching %s.\nChecking every %s. I'll ping you when a seat opens.\n\n%s",
			cfg.target(), cfg.Interval, helpText))
	}

	// checkResult carries one poll's outcome for both broadcasting and /check replies.
	type checkResult struct {
		err       error
		notFound  bool
		diag      string
		ride      *ride
		total     int
		breakdown string
		available bool
	}

	// poll performs one fetch+parse+match, updates state, and returns the result.
	poll := func() checkResult {
		var res checkResult
		body, err := fetch(client, req)
		state.mu.Lock()
		state.checks++
		state.lastCheck = time.Now()
		state.mu.Unlock()

		ts := time.Now().Format("15:04:05")
		if err != nil {
			res.err = err
			fmt.Printf("[%s] fetch error: %v\n", ts, err)
			state.mu.Lock()
			state.lastResult = "fetch error: " + err.Error()
			state.mu.Unlock()
			return res
		}

		rides, perr := parseRides(body)
		if perr != nil {
			res.err = perr
			fmt.Printf("[%s] parse error: %v\n", ts, perr)
			if firstRun {
				snippet := string(body)
				if len(snippet) > 500 {
					snippet = snippet[:500]
				}
				fmt.Println("    raw response head:", snippet)
			}
			state.mu.Lock()
			state.lastResult = "parse error: " + perr.Error()
			state.mu.Unlock()
			firstRun = false
			return res
		}

		if firstRun {
			fmt.Printf("[i] first check — trains returned: %s\n", summarize(rides))
		}
		firstRun = false

		r, ok := findRide(rides, cfg)
		if !ok {
			res.notFound = true
			res.diag = diagnose(rides, cfg)
			fmt.Printf("[%s] %s\n", ts, res.diag)
			state.mu.Lock()
			state.lastResult = res.diag
			state.available = false
			state.mu.Unlock()
			return res
		}

		total, lines := seatInfo(r)
		res.ride, res.total = r, total
		res.breakdown = strings.Join(lines, ", ")
		if res.breakdown == "" {
			res.breakdown = "no classes on sale"
		}
		res.available = total > 0

		state.mu.Lock()
		state.available = res.available
		if res.available {
			state.lastResult = fmt.Sprintf("AVAILABLE — %d seats (%s)", total, res.breakdown)
		} else {
			state.lastResult = "no seats (sold out)"
		}
		state.mu.Unlock()

		if res.available {
			fmt.Printf("[%s] N%d @ %s — SEATS AVAILABLE: %d (%s)\n", ts, r.RideNumber, r.hhmm(), total, res.breakdown)
		} else {
			fmt.Printf("[%s] N%d @ %s — no seats (sold out)\n", ts, r.RideNumber, r.hhmm())
		}
		return res
	}

	alertText := func(res checkResult) string {
		r := res.ride
		return fmt.Sprintf("✅ Seats available — N%d %s→%s at %s on %s\n\n%s\nTotal: %d\n\nBook now: %s",
			r.RideNumber, r.RideStartStation.Station.Name, r.RideEndStation.Station.Name,
			r.hhmm(), cfg.Date, res.breakdown, res.total, cfg.bookURL())
	}

	// statusFor formats a poll result as a reply to a /check request.
	statusFor := func(res checkResult) string {
		switch {
		case res.err != nil:
			return "⚠️ check failed: " + res.err.Error()
		case res.notFound:
			return "⚠️ " + res.diag
		case res.available:
			return alertText(res)
		default:
			return fmt.Sprintf("N%d @ %s on %s — no seats yet (sold out).",
				res.ride.RideNumber, res.ride.hhmm(), cfg.Date)
		}
	}

	// doCheck runs a poll and broadcasts alerts to subscribers (throttled).
	doCheck := func() {
		res := poll()
		switch {
		case res.err != nil:
			// logged in poll
		case res.notFound:
			if !warnedNotFound {
				warnedNotFound = true
				tg.broadcast(reg.targets(true), "⚠️ "+res.diag)
			}
		default:
			warnedNotFound = false
			if res.available {
				state.mu.Lock()
				should := time.Since(state.lastAlert) > cfg.AlertRepeat
				if should {
					state.lastAlert = time.Now()
				}
				state.mu.Unlock()
				if should {
					tg.broadcast(reg.targets(true), alertText(res))
				}
			}
		}
	}

	// command handler — any interaction subscribes the chat.
	go tg.listen(ctx, func(chatID, cmd string) {
		switch cmd {
		case "/start":
			reg.update(chatID, func(s *subscriber) { s.Muted = false })
			tg.sendTo(chatID, fmt.Sprintf("👋 Subscribed. Watching %s every %s.\n\n%s",
				cfg.target(), cfg.Interval, helpText))
		case "/help":
			reg.update(chatID, nil)
			tg.sendTo(chatID, helpText)
		case "/stop":
			reg.update(chatID, func(s *subscriber) { s.Muted = true })
			tg.sendTo(chatID, "🔕 Paused. No more seat alerts here. Send /continue to resume.")
		case "/continue":
			reg.update(chatID, func(s *subscriber) { s.Muted = false })
			tg.sendTo(chatID, "🔔 Resumed. You'll receive seat alerts again.")
		case "/heartbeat_off":
			reg.update(chatID, func(s *subscriber) { s.HeartbeatOff = true })
			tg.sendTo(chatID, "⏱🚫 Heartbeat off for this chat. (/heartbeat_on to re-enable)")
		case "/heartbeat_on":
			reg.update(chatID, func(s *subscriber) { s.HeartbeatOff = false })
			tg.sendTo(chatID, "⏱ Heartbeat on for this chat.")
		case "/status":
			reg.update(chatID, nil)
			n, last, res, avail := state.snapshot()
			when := "never"
			if !last.IsZero() {
				when = last.Format("15:04:05") + fmt.Sprintf(" (%s ago)", time.Since(last).Round(time.Second))
			}
			s := reg.get(chatID)
			tg.sendTo(chatID, fmt.Sprintf("Target: %s\nChecks: %d\nLast: %s\nResult: %s\nAvailable now: %v\n\nYour seat alerts: %s | heartbeat: %s",
				cfg.target(), n, when, res, avail, onoff(!s.Muted), onoff(!s.HeartbeatOff)))
		case "/check":
			reg.update(chatID, nil)
			tg.sendTo(chatID, "Checking now…")
			go func() { tg.sendTo(chatID, statusFor(poll())) }()
		default:
			tg.sendTo(chatID, "Unknown command.\n"+helpText)
		}
	})

	// optional heartbeat
	if cfg.HeartbeatEver > 0 {
		go func() {
			t := time.NewTicker(cfg.HeartbeatEver)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					n, _, res, _ := state.snapshot()
					tg.broadcast(reg.targets(false),
						fmt.Sprintf("⏱ Still watching %s — %d checks, latest: %s", cfg.target(), n, res))
				}
			}
		}()
	}

	// first check immediately, then on the interval
	doCheck()
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n[i] shutting down.")
			tg.broadcast(reg.targets(true), "👋 Bot stopped.")
			return
		case <-ticker.C:
			doCheck()
		}
	}
}
