# jarvis-bot

One Telegram bot, several independent parsers. Each parser runs its own poll
loop and its own set of chat commands; a shared, persisted subscriber registry
lets every chat opt in to whichever parsers it wants and receive only that
parser's messages.

## Parsers

- **nike** — polls Nike's availability API for a product group and alerts when a
  target size restocks.
- **gr** — polls gr.com.ge for free seats on Tbilisi↔Batumi trains. Each chat
  creates its own searches from Telegram (direction + date + departure time
  and/or train number) and can run several at once; searches persist to
  `GR_SEARCHES_FILE` (`gr_searches.json`).

## Telegram Mini App

The GR parser has a button-driven Mini App (no typing): pick a direction → pick
a date → tap a train from the live list to start watching it. Active watches are
managed in the same app (check now / stop / book link); seat alerts still arrive
as chat messages from the watcher loop.

Setup:

1. Telegram requires the app to be served over **public HTTPS**. The bot serves
   it on `WEBAPP_ADDR` (default `:8090`); front that with a reverse proxy, or
   for development a tunnel:

   ```sh
   cloudflared tunnel --url http://localhost:8090
   # or: ngrok http 8090
   ```

2. Set `WEBAPP_URL=https://<your-public-host>` in `.env` and restart.

The bot then sets its menu button (next to the message input) to open the app,
and `/app` (or `/start`) sends an "Open Train Watch" button. Requests are
authenticated with the Mini App `initData` signature, so the backend knows the
Telegram user without any login. Unset `WEBAPP_URL` disables the app; text
commands keep working either way.

## Layout

```
main.go                # config, Telegram listener, command dispatch
internal/
  tg/                  # Telegram Bot API client (send + long-poll + web_app buttons)
  subs/                # per-chat, per-parser subscriber registry (JSON-persisted)
  parser/              # Parser interface (Name/Run/Handle/StatusLine)
  nike/                # Nike parser
  gr/                  # GR ticket parser (+ api.go: structured API for the Mini App)
  webapp/              # Mini App: static frontend (embedded) + JSON API + initData auth
```

## Run

```sh
cp .env.example .env    # fill in TELEGRAM_BOT_TOKEN + each parser's config
go run .
```

A parser whose required config is missing is skipped with a warning, so you can
run just one. The bot needs at least one configured parser.

## Telegram commands

- `/start`, `/help` — list parsers and their commands
- `/status` — state of all parsers
- Per parser (`nike`, `gr`):
  - `/<parser>_start` — subscribe and show current state
  - `/<parser>_stop` / `/<parser>_continue` — pause / resume alerts
  - `/<parser>_status` — current state
  - `/<parser>_check` — check right now
- GR only:
  - `/gr_trains tb|bt <date>` — list trains, times and seats
    (`tb` = Tbilisi→Batumi, `bt` = Batumi→Tbilisi; date = `YYYY-MM-DD`,
    `today` or `tomorrow`)
  - `/gr_search tb|bt <date> <HH:MM and/or train №>` — start watching a train;
    repeat for as many concurrent searches as you like, e.g.
    `/gr_search tb 2026-08-20 06:00`, `/gr_search tb 2026-08-20 08:00`,
    `/gr_search bt 2026-08-21 14:00`
  - `/gr_list` — your active searches (with ids)
  - `/gr_cancel <id|all>` — stop a search
  - `/gr_check [id]` — poll now
  - `/gr_heartbeat_off`, `/gr_heartbeat_on`

Subscriptions and per-chat flags persist to `SUBSCRIBERS_FILE`
(`subscribers.json`) and survive restarts.

## Roadmap

GR now takes its searches from Telegram. Next: same for nike (e.g.
`/nike_watch <group> <style> <sizes>`) instead of the process-wide env config.
