# CLAUDE.md

Guidance for working in this repo. Keep it current when build/test/deploy or architecture changes.

## What this is

**ACR Alpine Weather** — a serverless Go bridge between Garmin inReach satellite devices and NZ alpine weather/avalanche APIs. A user emails/satellite-messages `UPDATE` to the bot; the bot fetches hyper-local weather (yr.no, MetService, NZ Avalanche Advisory), compresses it to <160 chars, and replies back onto the device via Garmin's `explore.garmin.com` web form. See `README.md` for the product story and payload format.

## Layout

- `cmd/bot/main.go` — the **weather-bot** Lambda. Runs every minute (Netlify cron). Polls Gmail IMAP, parses inbound messages, fetches weather, replies to Garmin. This is the heart of the system.
- `cmd/weather-api/main.go` — the **weather-api** HTTP Lambda. Serves `GET /weather-api?lat&lon`, `/weather-api/all`, `/log` (usage log), and `/debug` (JSON debug logs for the live viewer).
- `cmd/test-email/main.go` — small email-sending utility.
- `internal/forecast/forecast.go` — weather sourcing, geofencing (`Parks`), report building (`BuildReport`, `BuildAllReports`, `GetClosestPark`, `GetElevation`, `FetchMetService`, `FetchAvalanche`).
- `public/` — static site. `index.html` (user guide), `debug.html` (live log viewer).
- `functions/` — build output (git-ignored); Netlify deploys these.
- `scripts/init.sh` (deps + .env scaffold), `scripts/build.sh` (cross-compile to linux/amd64).
- `netlify.toml` — function schedule + friendly-path redirects.

## Build / run / test

Requires **Go 1.23+** (see `go.mod`). `CGO_ENABLED=0` static binaries.

```bash
./scripts/build.sh                 # cross-compiles both functions into functions/
go test ./cmd/bot/                 # fast unit tests (no network/IMAP) — parse + routing logic
go test -tags integration ./...    # live tests: needs Gmail + Turso creds + network
```

Run a single bot poll locally (loads .env, one handler cycle):
```bash
export $(grep -v '^#' .env | xargs) && LOCAL_WEATHER_BOT=1 ./functions/weather-bot
```
Run the API + static viewer locally on :9090:
```bash
LOCAL_WEATHER_API=1 ./functions/weather-api    # http://localhost:9090/debug.html
```

## State & persistence (Turso / libsql)

Single SQLite-at-the-edge DB. Tables created in `ensureSchema` (bot):
- `session_state` — one row (`id='garmin_primary'`): last coords, park, `ext_id`/`guid` (Garmin session tokens for routine broadcasts), `last_fetch`, `last_routine_nz` (broadcast dedupe slot).
- `request_log` — usage log, rendered by `/log`.
- `debug_log` — captured `log.*` output per invocation, streamed to `/debug.html`. Pruned to ~2 days.

Env: `TURSO_DB_URL`, `TURSO_AUTH_TOKEN`, `EMAIL_USER`, `EMAIL_PASS` (Gmail app password). Optional: `LOGS_KEY` (protects `/log` + `/debug`), `GARMIN_DRY_RUN`, `GARMIN_DRY_RUN_REPLY_TO`.

## The Garmin reply flow (the fragile part)

1. Inbound Garmin email arrives from `no.reply.inreach@garmin.com`, subject `inReach message from <device>`, **single-part `text/plain; charset=us-ascii`, quoted-printable**. Body = the typed command, then fixed boilerplate:
   ```
   Update

   View the location or send a reply to <device>:
   https://inreachlink.com/<token>

   <device> sent this message from: Lat -44.988456 Lon 168.904967

   Do not reply directly to this message. ...
   ```
   A captured real example lives in `cmd/bot/testdata/garmin_update.eml` (golden test fixture).
2. `extractEmailBody` decodes MIME/quoted-printable to plain text.
3. `parseGarminBody` extracts the `inreachlink.com` shortlink, coords, and command. **Commands are detected only in `userMessage(body)`** (text before the boilerplate) so device names/boilerplate can't trigger false START/STOP/UPDATE.
4. `InitGarminSession(shortlink)` follows the shortlink → redirects to `explore.garmin.com/TextMessage/TxtMsg?extId=&guid=`, captures cookies + the hidden `__RequestVerificationToken` CSRF token. **This is where anti-bot/WAF failures happen.**
5. `SendGarminReply` POSTs the (chunked, <160 char) reply back to that form. It now reads the response body and flags a `200`-with-error-page via `looksLikeGarminError` — a plain 200 is NOT proof of delivery.
6. **All delivery goes through `sendToGarmin`.** With `GARMIN_DRY_RUN=1` it emails the payload to `GARMIN_DRY_RUN_REPLY_TO` instead of POSTing — use this to test the full pipeline without a device or satellite credits. `InitGarminSession` still runs in dry-run (it's a harmless GET), so you can test the anti-bot handshake without actually sending.

Non-Garmin senders go through a separate human "test" path (`parseTestCommand` → `sendTestEmailReply`): `update lat:.., long:..`, bare `update` (uses last-known coords), or `all`.

## Conventions & gotchas

- **Match existing style**: emoji-prefixed log lines (`✅ ❌ ⚠️ 🚀 🧭 🧮 📍 🔗 📤 📨`), terse error handling, `log.Printf`. The `/debug.html` viewer classifies level from these.
- **Logging is the debugging tool.** The bot is serverless — you can't tail stdout. Every line is captured to `debug_log` and streamed to `/debug.html`. When tracing a failure, add `log.Printf` and watch the page (with `?key=<LOGS_KEY>`).
- The bot's `handler` installs a `logCapture` writer and `defer`s a flush to `debug_log`; `log.Fatalf` paths skip the flush (no DB / pre-flush only).
- IMAP: drain `Fetch` fully before issuing `Store` (mark-seen) — interleaving deadlocks. On SMTP/send failure, messages are left unread for retry.
- Garmin shortlinks appear single-use/expiring; routine 07:00/19:00 broadcasts reuse stored `ext_id`/`guid` via `InitGarminSessionFromState`.
- Replies to the device only work via the web form — email replies bounce (`Do not reply directly`).
- Pure parsing/routing helpers in `cmd/bot/main.go` (`isGarminSender`, `parseTestCommand`, `parseGarminBody`, `userMessage`, `splitForGarmin`, `looksLikeGarminError`) are unit-tested in `cmd/bot/parse_test.go` — keep them pure (no I/O) so they stay testable.

## Local toolchain note

A dev machine here had only Go 1.15 installed, which **cannot** build this Go 1.23 module (`invalid go version '1.23.1'`). `gofmt -e` parse-checks fine, but real `go build`/`go test` must run with Go 1.23+ (or in Netlify CI).
