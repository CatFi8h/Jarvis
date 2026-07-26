// nike-monitor polls Nike's discover API for a product group and sends a
// Telegram notification when a target size comes back in stock.
//
// Endpoint (one call gives sizes + availability for all colorways in the group):
//
//	GET https://api.nike.com/discover/product_details_availability/v1/
//	    marketplace/{mp}/language/{lang}/consumerChannelId/{ch}/groupKey/{key}
//
// Response shape:
//
//	{"sizes":[{"label":"10","localizedLabel":"M 10 / W 11.5",
//	           "productCode":"IF2857-600","merchSkuId":"...",
//	           "availability":{"isAvailable":true,"ship":"HIGH"}}, ...]}
//
// Config (env vars):
//
//	GROUP_KEY           required, e.g. GbnAW5Hb (last path segment of the
//	                    availability URL in DevTools)
//	STYLE_COLOR         required, e.g. IF2857-600 (matched against productCode)
//	TARGET_SIZES        required, comma-separated "label" values, e.g. "10,10.5",
//	                    or "*" for all
//	TELEGRAM_BOT_TOKEN  required
//	TELEGRAM_CHAT_ID    required
//	MARKETPLACE         default US
//	LANGUAGE            default en
//	POLL_INTERVAL       default 10m
//	NOTIFY_ON_START     default true
package main

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
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

const (
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
	TgToken       string
	TgChatID      string
	NotifyOnStart bool
}

func loadConfig() (config, error) {
	_ = godotenv.Load()
	c := config{
		GroupKey:      os.Getenv("GROUP_KEY"),
		StyleColor:    strings.ToUpper(os.Getenv("STYLE_COLOR")),
		Marketplace:   envOr("MARKETPLACE", "US"),
		Language:      envOr("LANGUAGE", "en"),
		TgToken:       os.Getenv("TELEGRAM_BOT_TOKEN"),
		TgChatID:      os.Getenv("TELEGRAM_CHAT_ID"),
		NotifyOnStart: envOr("NOTIFY_ON_START", "true") == "true",
	}
	if c.GroupKey == "" {
		return c, errors.New("GROUP_KEY is required (e.g. GbnAW5Hb)")
	}
	if c.StyleColor == "" {
		return c, errors.New("STYLE_COLOR is required (e.g. IF2857-600)")
	}
	if c.TgToken == "" || c.TgChatID == "" {
		return c, errors.New("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID are required")
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

// ---------- Nike API ----------

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
	seen := map[string]bool{}
	for _, s := range ar.Sizes {
		if !strings.EqualFold(s.ProductCode, styleColor) {
			continue
		}
		seen[s.ProductCode] = true
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

type monitor struct {
	cfg    config
	client *http.Client

	prev     map[string]sizeStatus // last known state per size label
	failures int
	warned   bool
}

func (m *monitor) fetch(ctx context.Context) ([]sizeStatus, error) {
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

func (m *monitor) watched(size string) bool {
	return m.cfg.TargetSizes == nil || m.cfg.TargetSizes[size]
}

func (m *monitor) poll(ctx context.Context) {
	statuses, err := m.fetch(ctx)
	if err != nil {
		m.failures++
		log.Printf("poll failed (%d in a row): %v", m.failures, err)
		if m.failures >= maxConsecutiveFailures && !m.warned {
			m.warned = true
			m.notify(ctx, fmt.Sprintf("⚠️ nike-monitor: %d polls in a row failed, last error: %v", m.failures, err))
		}
		return
	}
	m.failures, m.warned = 0, false

	curr := make(map[string]sizeStatus, len(statuses))
	for _, s := range statuses {
		curr[s.Size] = s
	}

	if m.prev == nil {
		m.prev = curr
		log.Printf("baseline: %s", strings.ReplaceAll(summarize(statuses, m.watched), "\n", " | "))
		if m.cfg.NotifyOnStart {
			m.notify(ctx, fmt.Sprintf("👟 Monitoring %s\nWatching sizes: %s\nCurrent stock:\n%s",
				m.cfg.StyleColor, m.targetsLabel(), summarize(statuses, m.watched)))
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

	if len(restocked) > 0 {
		sort.Strings(restocked)
		m.notify(ctx, fmt.Sprintf("🔥 BACK IN STOCK — %s\n%s\nhttps://www.nike.com/t/-/%s",
			m.cfg.StyleColor, strings.Join(restocked, "\n"), m.cfg.StyleColor))
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		log.Printf("went out of stock: %s", strings.Join(gone, ", "))
	}
}

func (m *monitor) targetsLabel() string {
	if m.cfg.TargetSizes == nil {
		return "all"
	}
	return strings.Join(sortedKeys(m.cfg.TargetSizes), ", ")
}

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

// ---------- telegram ----------

func (m *monitor) notify(ctx context.Context, text string) {
	api := "https://api.telegram.org/bot" + m.cfg.TgToken + "/sendMessage"
	form := url.Values{
		"chat_id":                  {m.cfg.TgChatID},
		"text":                     {text},
		"disable_web_page_preview": {"true"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api, strings.NewReader(form.Encode()))
	if err != nil {
		log.Printf("telegram: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.client.Do(req)
	if err != nil {
		log.Printf("telegram: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		log.Printf("telegram: HTTP %d: %s", resp.StatusCode, string(body))
	}
}

// ---------- helpers ----------

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

// ---------- main ----------

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[nike-monitor] ")

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	m := &monitor{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("watching group %s / style %s, sizes: %s, every %s",
		cfg.GroupKey, cfg.StyleColor, m.targetsLabel(), cfg.Interval)
	m.poll(ctx)

	for {
		jitter := time.Duration(rand.Int63n(int64(cfg.Interval)/5)) - cfg.Interval/10
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-time.After(cfg.Interval + jitter):
			m.poll(ctx)
		}
	}
}
