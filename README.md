# jarvis-bot

One Telegram bot, several independent parsers. Each parser runs its own poll
loop and its own set of chat commands; a shared, persisted subscriber registry
lets every chat opt in to whichever parsers it wants and receive only that
parser's messages.

## Parsers

- **nike** — polls Nike's availability API for a product group and alerts when a
  target size restocks.
- **gr** — polls gr.com.ge for a free seat on a specific train (date + time
  and/or train number).

## Layout

```
main.go                # config, Telegram listener, command dispatch
internal/
  tg/                  # Telegram Bot API client (send + long-poll)
  subs/                # per-chat, per-parser subscriber registry (JSON-persisted)
  parser/              # Parser interface (Name/Run/Handle/StatusLine)
  nike/                # Nike parser
  gr/                  # GR ticket parser
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
- GR only: `/gr_heartbeat_off`, `/gr_heartbeat_on`

Subscriptions and per-chat flags persist to `SUBSCRIBERS_FILE`
(`subscribers.json`) and survive restarts.

## Roadmap

Next: let chats insert their own search data from Telegram (e.g.
`/nike_watch <group> <style> <sizes>`, `/gr_watch <date> <train>`) instead of the
process-wide env config. The command layer already carries an `args` string and
the registry is keyed per chat, so this slots in without reshaping the design.
