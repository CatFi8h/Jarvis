// Package nike is the Nike stock parser: it polls Nike's discover availability
// API for a product group and alerts subscribed chats when a target size comes
// back in stock.
//
// Endpoint (one call gives sizes + availability for all colorways in the group):
//
//	GET https://api.nike.com/discover/product_details_availability/v1/
//	    marketplace/{mp}/language/{lang}/consumerChannelId/{ch}/groupKey/{key}
//
// Config (env vars):
//
//	GROUP_KEY       required, e.g. GbnAW5Hb (last path segment of the URL in DevTools)
//	STYLE_COLOR     required, e.g. IF2857-600 (matched against productCode)
//	TARGET_SIZES    required, comma-separated labels e.g. "10,10.5", or "*" for all
//	MARKETPLACE     default US
//	LANGUAGE        default en
//	POLL_INTERVAL   default 10m
//	NOTIFY_ON_START default true
package nike

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"jarvis-bot/internal/subs"
	"jarvis-bot/internal/tg"
)

const (
	name         = "nike"
	apiBase      = "https://api.nike.com/discover/product_details_availability/v1"
	webChannelID = "d9a5bc42-4b9c-4976-858a-f159cf99c647" // nike.com web channel
	userAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	nikeCallerID = "com.nike.commerce.nikedotcom.web" // required header value

	maxConsecutiveFailures = 5
)

// ---------- config ----------

type config struct {
	GroupKey      string
	StyleColor    string
	TargetSizes   map[string]bool // nil means all sizes
	Marketplace   string
	Language      string
	Interval      time.Duration
	NotifyOnStart bool
}

func loadConfig() (config, error) {
	c := config{
		GroupKey:      os.Getenv("GROUP_KEY"),
		StyleColor:    strings.ToUpper(os.Getenv("STYLE_COLOR")),
		Marketplace:   envOr("MARKETPLACE", "US"),
		Language:      envOr("LANGUAGE", "en"),
		NotifyOnStart: envOr("NOTIFY_ON_START", "true") == "true",
	}
	if c.GroupKey == "" {
		return c, errors.New("GROUP_KEY is required (e.g. GbnAW5Hb)")
	}
	if c.StyleColor == "" {
		return c, errors.New("STYLE_COLOR is required (e.g. IF2857-600)")
	}

	sizes := envOr("TARGET_SIZES", "")
	if sizes == "" {
		return c, errors.New(`TARGET_SIZES is required (e.g. "10,10.5" or "*")`)
	}
	if sizes != "*" {
		c.TargetSizes = map[string]bool{}
		for _, s := range strings.Split(sizes, ",") {
			if s = strings.TrimSpace(s); s != "" {
				c.TargetSizes[s] = true
			}
		}
	}

	iv, err := time.ParseDuration(envOr("POLL_INTERVAL", "10m"))
	if err != nil {
		return c, fmt.Errorf("bad POLL_INTERVAL: %w", err)
	}
	c.Interval = iv
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------- Nike API types ----------

type availResponse struct {
	GroupKey string      `json:"groupKey"`
	Sizes    []sizeEntry `json:"sizes"`
}

type sizeEntry struct {
	Label          string `json:"label"`
	LocalizedLabel string `json:"localizedLabel"`
	ProductCode    string `json:"productCode"`
	MerchSkuID     string `json:"merchSkuId"`
	Availability   struct {
		IsAvailable bool   `json:"isAvailable"`
		Ship        string `json:"ship"`
	} `json:"availability"`
}

type sizeStatus struct {
	Size      string // "label", e.g. "10"
	Localized string // e.g. "M 10 / W 11.5"
	Available bool
	Ship      string // HIGH / MEDIUM / LOW / OOS
}

func parseSizes(body []byte, styleColor string) ([]sizeStatus, error) {
	var ar availResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("decode: %w (body: %s)", err, truncate(string(body), 300))
	}

	var out []sizeStatus
	for _, s := range ar.Sizes {
		if !strings.EqualFold(s.ProductCode, styleColor) {
			continue
		}
		out = append(out, sizeStatus{
			Size:      s.Label,
			Localized: s.LocalizedLabel,
			Available: s.Availability.IsAvailable,
			Ship:      s.Availability.Ship,
		})
	}
	if len(out) == 0 {
		codes := map[string]bool{}
		for _, s := range ar.Sizes {
			codes[s.ProductCode] = true
		}
		return nil, fmt.Errorf("style %s not found in group; styles present: %s",
			styleColor, strings.Join(sortedKeys(codes), ", "))
	}
	sort.Slice(out, func(i, j int) bool { return sizeLess(out[i].Size, out[j].Size) })
	return out, nil
}

// ---------- monitor ----------

// Monitor is the Nike parser. It implements parser.Parser.
type Monitor struct {
	cfg    config
	client *http.Client
	reg    *subs.Registry
	tg     *tg.Telegram

	mu       sync.Mutex            // guards prev, failures, warned, stats
	prev     map[string]sizeStatus // last known state per size label
	failures int
	warned   bool

	checks     int
	lastCheck  time.Time
	lastResult string
}

// New builds the Nike monitor, loading its config from the environment. It
// returns nil (with error) if required config is missing, so the bot can skip
// this parser and still run others.
func New(reg *subs.Registry, t *tg.Telegram) (*Monitor, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return &Monitor{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		reg:    reg,
		tg:     t,
	}, nil
}

func (m *Monitor) Name() string { return name }

func (m *Monitor) fetch(ctx context.Context) ([]sizeStatus, error) {
	u := fmt.Sprintf("%s/marketplace/%s/language/%s/consumerChannelId/%s/groupKey/%s",
		apiBase, m.cfg.Marketplace, m.cfg.Language, webChannelID, url.PathEscape(m.cfg.GroupKey))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://www.nike.com/")
	req.Header.Set("nike-api-caller-id", nikeCallerID)

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nike api: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return parseSizes(body, m.cfg.StyleColor)
}

func (m *Monitor) watched(size string) bool {
	return m.cfg.TargetSizes == nil || m.cfg.TargetSizes[size]
}

// broadcast sends an alert to every chat subscribed to nike (respecting mutes).
func (m *Monitor) broadcast(text string) {
	m.tg.Broadcast(m.reg.Targets(name, true), text)
}

// poll performs one diffing check (from the Run loop) and alerts on restocks.
func (m *Monitor) poll(ctx context.Context) {
	statuses, err := m.fetch(ctx)

	m.mu.Lock()
	m.checks++
	m.lastCheck = time.Now()
	if err != nil {
		m.failures++
		m.lastResult = "fetch error: " + err.Error()
		log.Printf("[nike] poll failed (%d in a row): %v", m.failures, err)
		firedWarn := m.failures >= maxConsecutiveFailures && !m.warned
		if firedWarn {
			m.warned = true
		}
		m.mu.Unlock()
		if firedWarn {
			m.broadcast(fmt.Sprintf("⚠️ nike: %d polls in a row failed, last error: %v", maxConsecutiveFailures, err))
		}
		return
	}
	m.failures, m.warned = 0, false

	curr := make(map[string]sizeStatus, len(statuses))
	for _, s := range statuses {
		curr[s.Size] = s
	}
	m.lastResult = strings.ReplaceAll(summarize(statuses, m.watched), "\n", " | ")

	if m.prev == nil {
		m.prev = curr
		notifyOnStart := m.cfg.NotifyOnStart
		style, label := m.cfg.StyleColor, m.targetsLabel()
		summary := summarize(statuses, m.watched)
		m.mu.Unlock()
		log.Printf("[nike] baseline: %s", strings.ReplaceAll(summary, "\n", " | "))
		if notifyOnStart {
			m.broadcast(fmt.Sprintf("👟 Monitoring %s\nWatching sizes: %s\nCurrent stock:\n%s", style, label, summary))
		}
		return
	}

	var restocked, gone []string
	for size, s := range curr {
		if !m.watched(size) {
			continue
		}
		p, known := m.prev[size]
		switch {
		case s.Available && (!known || !p.Available):
			restocked = append(restocked, fmt.Sprintf("%s (ship: %s)", s.Localized, orDash(s.Ship)))
		case !s.Available && known && p.Available:
			gone = append(gone, size)
		}
	}
	m.prev = curr
	style := m.cfg.StyleColor
	m.mu.Unlock()

	if len(restocked) > 0 {
		sort.Strings(restocked)
		m.broadcast(fmt.Sprintf("🔥 BACK IN STOCK — %s\n%s\nhttps://www.nike.com/t/-/%s",
			style, strings.Join(restocked, "\n"), style))
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		log.Printf("[nike] went out of stock: %s", strings.Join(gone, ", "))
	}
}

// snapshot fetches current stock without touching the restock-diffing baseline;
// used for /nike_start and /nike_check replies to a single chat.
func (m *Monitor) snapshot(ctx context.Context) string {
	statuses, err := m.fetch(ctx)
	if err != nil {
		return "⚠️ nike check failed: " + err.Error()
	}
	return summarize(statuses, m.watched)
}

// Run is the independent poll loop: immediate check, then jittered interval.
func (m *Monitor) Run(ctx context.Context) {
	log.Printf("[nike] watching group %s / style %s, sizes: %s, every %s",
		m.cfg.GroupKey, m.cfg.StyleColor, m.targetsLabel(), m.cfg.Interval)
	m.poll(ctx)
	for {
		jitter := time.Duration(rand.Int63n(int64(m.cfg.Interval)/5)) - m.cfg.Interval/10
		select {
		case <-ctx.Done():
			log.Println("[nike] shutting down")
			return
		case <-time.After(m.cfg.Interval + jitter):
			m.poll(ctx)
		}
	}
}

// Handle processes /nike_<action> commands for one chat.
func (m *Monitor) Handle(chatID, action, args string) string {
	switch action {
	case "start":
		m.reg.Subscribe(chatID, name)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return fmt.Sprintf("👟 Subscribed to Nike. Watching %s (sizes: %s) every %s.\nCurrent stock:\n%s",
			m.cfg.StyleColor, m.targetsLabel(), m.cfg.Interval, m.snapshot(ctx))
	case "stop":
		m.reg.Mute(chatID, name, true)
		return "🔕 Nike alerts paused for this chat. Send /nike_continue to resume."
	case "continue":
		m.reg.Mute(chatID, name, false)
		return "🔔 Nike alerts resumed."
	case "status":
		return m.StatusLine(chatID)
	case "check":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return "Checking Nike now…\n" + m.snapshot(ctx)
	default:
		return ""
	}
}

// StatusLine reports the Nike parser state for a chat.
func (m *Monitor) StatusLine(chatID string) string {
	m.mu.Lock()
	checks, last, res := m.checks, m.lastCheck, m.lastResult
	m.mu.Unlock()
	when := "never"
	if !last.IsZero() {
		when = last.Format("15:04:05") + fmt.Sprintf(" (%s ago)", time.Since(last).Round(time.Second))
	}
	if res == "" {
		res = "—"
	}
	p := m.reg.Get(chatID, name)
	return fmt.Sprintf("👟 Nike — %s (sizes: %s)\nChecks: %d\nLast: %s\nStock: %s\nYour subscription: %s | alerts: %s",
		m.cfg.StyleColor, m.targetsLabel(), checks, when, res,
		onoff(p.Subscribed), onoff(p.Subscribed && !p.Muted))
}

func (m *Monitor) targetsLabel() string {
	if m.cfg.TargetSizes == nil {
		return "all"
	}
	return strings.Join(sortedKeys(m.cfg.TargetSizes), ", ")
}

// ---------- helpers ----------

func summarize(statuses []sizeStatus, watched func(string) bool) string {
	var b strings.Builder
	for _, s := range statuses {
		if !watched(s.Size) {
			continue
		}
		mark := "❌"
		if s.Available {
			mark = "✅"
		}
		fmt.Fprintf(&b, "%s %s (%s)\n", mark, s.Localized, orDash(s.Ship))
	}
	if b.Len() == 0 {
		return "(none of the watched sizes exist for this style)"
	}
	return strings.TrimRight(b.String(), "\n")
}

func onoff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sizeLess sorts numeric labels numerically ("9.5" < "10"), falling back to
// string comparison for non-numeric labels.
func sizeLess(a, b string) bool {
	var fa, fb float64
	_, errA := fmt.Sscanf(a, "%f", &fa)
	_, errB := fmt.Sscanf(b, "%f", &fb)
	if errA == nil && errB == nil {
		return fa < fb
	}
	return a < b
}
