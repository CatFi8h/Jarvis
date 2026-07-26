// jarvis-bot is a single Telegram bot that drives several independent parsers.
// Each parser runs its own poll loop and owns a set of namespaced commands
// (/nike_start, /gr_check, …). One bot token allows exactly one getUpdates
// listener, so the process runs one Telegram listener that routes commands to
// the addressed parser, while a shared subscriber registry lets each chat opt in
// to whichever parsers it wants and receive that parser's messages only.
//
// All wiring and lifecycle live in internal/bot; main is just the entrypoint.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"jarvis-bot/internal/bot"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[jarvis] ")

	_ = godotenv.Load()

	b, err := bot.New()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b.Run(ctx)
	log.Println("shutting down")
}
