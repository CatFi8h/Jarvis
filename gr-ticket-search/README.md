# gr-ticket-bot

Telegram bot that watches **gr.com.ge** for a free seat on one specific train
(by date + time) and messages you the moment one appears — e.g. when someone
returns a ticket. It never buys anything; it just tells you to.

Works out of the box: it POSTs to `https://gr.com.ge/api/ticket-search` with a
JSON body built from your `.env`. No request capture needed.

## Setup

1. **Install Go** (1.21+), then in this folder:
   ```bash
   cp 12.env.example .env
   ```
2. **Fill in `.env`** — Telegram token/chat id, the date/time/train, interval.
   That's it for the common case.
3. **Run it:**
   ```bash
   go run .
   # or build a single binary:
   go build -o gr-ticket-bot . && ./gr-ticket-bot
   ```

On first run it prints the matched train object so you can confirm the fields.
If the free-seat count looks wrong, set `FREE_SEATS_KEYS` in `.env` to the exact
key(s) you see in that dump.

## Telegram commands
- `/status` — checks done, last result, whether seats are available now
- `/check` — force an immediate check
- `/start` — quick help

## If the endpoint ever needs auth
If gr.com.ge starts requiring cookies/headers, capture the real request in
Chrome DevTools (Network → the `ticket-search` request → right-click → **Copy as
cURL**), paste it into `request.curl`, and the bot uses it instead — still
forcing your `DEPARTURE_DATE` into it.

## Run it 24/7
Single static binary. Keep it alive with `nohup ./gr-ticket-bot &`, a `tmux`
pane, or a small `systemd` / `launchd` unit.
