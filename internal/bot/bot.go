// Package bot is the application service: it loads config, wires the shared
// Telegram client, subscriber registry and parsers, and owns the runtime
// lifecycle (parser goroutines + the single command listener). main is just an
// entrypoint that constructs a Bot and runs it.
package bot

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"jarvis-bot/internal/gr"
	"jarvis-bot/internal/nike"
	"jarvis-bot/internal/parser"
	"jarvis-bot/internal/subs"
	"jarvis-bot/internal/tg"
	"jarvis-bot/internal/webapp"
)

// Bot wires the Telegram client, subscriber registry and configured parsers, and
// routes incoming commands to the addressed parser.
type Bot struct {
	tg      *tg.Telegram
	reg     *subs.Registry
	parsers map[string]parser.Parser
	help    string // precomputed help/usage text

	web    *webapp.Server // Mini App backend; nil when WEBAPP_URL is unset
	webURL string         // public HTTPS URL of the Mini App
}

// New is the composition root: it reads config from the environment, builds the
// shared dependencies, registers every parser whose config is present (skipping
// the rest), and returns a ready-to-run Bot. It errors if the token is missing
// or no parser is configured.
func New() (*Bot, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	subsFile := envOr("SUBSCRIBERS_FILE", "subscribers.json")
	bootstrapChat := os.Getenv("TELEGRAM_CHAT_ID")

	reg := subs.New(subsFile)
	tgc := &tg.Telegram{Token: token, HTTP: &http.Client{Timeout: 20 * time.Second}}

	// Register parsers. A parser whose config is missing/invalid is skipped with
	// a warning so the others still run — parsers are independent.
	parsers := map[string]parser.Parser{}
	if p, err := nike.New(reg, tgc); err != nil {
		log.Printf("nike parser disabled: %v", err)
	} else {
		parsers[p.Name()] = p
	}
	var grw *gr.Watcher
	if p, err := gr.New(reg, tgc); err != nil {
		log.Printf("gr parser disabled: %v", err)
	} else {
		parsers[p.Name()] = p
		grw = p
	}
	if len(parsers) == 0 {
		return nil, fmt.Errorf("no parsers configured — set at least one parser's env vars (see .env.example)")
	}

	// Optional bootstrap subscriber from .env (legacy single-chat setups).
	if bootstrapChat != "" {
		for n := range parsers {
			reg.Subscribe(bootstrapChat, n)
		}
	}

	b := &Bot{tg: tgc, reg: reg, parsers: parsers}

	// Optional Telegram Mini App: needs a public HTTPS URL (WEBAPP_URL) that
	// fronts the local listener (WEBAPP_ADDR) — e.g. a reverse proxy or a
	// cloudflared/ngrok tunnel during development.
	if webURL := os.Getenv("WEBAPP_URL"); webURL != "" {
		if grw == nil {
			log.Printf("webapp disabled: WEBAPP_URL is set but the gr parser is not configured")
		} else {
			b.webURL = webURL
			b.web = webapp.New(envOr("WEBAPP_ADDR", ":8090"), token, grw)
		}
	}

	b.help = b.helpText()
	return b, nil
}

// Run starts each parser's independent poll loop and then blocks on the single
// Telegram command listener until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	for _, p := range b.parsers {
		go b.runSafe(ctx, p)
	}
	if b.web != nil {
		go b.web.Run(ctx)
		if err := b.tg.SetMenuButton("🚆 Trains", b.webURL); err != nil {
			log.Printf("menu button: %v", err)
		}
	}
	log.Printf("started with parsers: %s", strings.Join(b.sortedNames(), ", "))
	b.tg.Listen(ctx, b.dispatch)
}

// runSafe runs a parser's loop, recovering from a panic so one parser crashing
// doesn't take down the others.
func (b *Bot) runSafe(ctx context.Context, p parser.Parser) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("parser %q panicked: %v", p.Name(), r)
		}
	}()
	p.Run(ctx)
}

// dispatch routes an incoming command to a global handler or the addressed
// parser. Namespaced commands are "/<parser>_<action> <args>".
func (b *Bot) dispatch(chatID, cmd, args string) {
	cmd = strings.TrimPrefix(cmd, "/")
	switch cmd {
	case "start", "help":
		b.tg.SendTo(chatID, b.help)
		b.sendAppButton(chatID)
		return
	case "app", "trains":
		if b.web != nil {
			b.sendAppButton(chatID)
		} else {
			b.tg.SendTo(chatID, "Mini App is not configured (WEBAPP_URL unset).")
		}
		return
	case "status":
		b.tg.SendTo(chatID, b.statusAll(chatID))
		return
	}

	if name, action, ok := strings.Cut(cmd, "_"); ok {
		if p, exists := b.parsers[name]; exists {
			if reply := p.Handle(chatID, action, args); reply != "" {
				b.tg.SendTo(chatID, reply)
				return
			}
		}
	}
	b.tg.SendTo(chatID, "Unknown command.\n\n"+b.help)
}

// sendAppButton offers the Mini App as an inline button (no-op if disabled).
func (b *Bot) sendAppButton(chatID string) {
	if b.web == nil {
		return
	}
	b.tg.SendWebAppButton(chatID,
		"🚆 Tap below to pick a direction, date and train with buttons — no typing needed:",
		"Open Train Watch", b.webURL)
}

// statusAll concatenates every parser's status line for the requesting chat.
func (b *Bot) statusAll(chatID string) string {
	var sb strings.Builder
	for _, n := range b.sortedNames() {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(b.parsers[n].StatusLine(chatID))
	}
	return sb.String()
}

func (b *Bot) helpText() string {
	var sb strings.Builder
	sb.WriteString("👋 Jarvis bot. Available parsers — subscribe to whichever you want:\n\n")
	if b.web != nil {
		sb.WriteString("🚆 Easiest way to watch trains: /app — opens the Train Watch Mini App (all buttons, no typing).\n\n")
	}
	for _, n := range b.sortedNames() {
		sb.WriteString("• " + n + ":\n")
		sb.WriteString("   /" + n + "_start – subscribe & show current state\n")
		sb.WriteString("   /" + n + "_stop – pause alerts\n")
		sb.WriteString("   /" + n + "_continue – resume alerts\n")
		sb.WriteString("   /" + n + "_status – current state\n")
		sb.WriteString("   /" + n + "_check – check right now\n")
		if n == "gr" {
			sb.WriteString("   /gr_trains tb|bt <date> – list trains & seats (tb = Tbilisi→Batumi, bt = Batumi→Tbilisi)\n")
			sb.WriteString("   /gr_search tb|bt <date> <HH:MM and/or train №> – watch a train (run several at once)\n")
			sb.WriteString("   /gr_list – your active searches\n")
			sb.WriteString("   /gr_cancel <id|all> – stop a search\n")
			sb.WriteString("   /gr_heartbeat_off | /gr_heartbeat_on – periodic pings\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("/status – state of all parsers")
	return sb.String()
}

func (b *Bot) sortedNames() []string {
	names := make([]string, 0, len(b.parsers))
	for n := range b.parsers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
