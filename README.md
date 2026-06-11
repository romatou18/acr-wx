# 🏔️ Garmin Inreach NZ Alpine Weather Bridge

A optimised serverless Go application that acts as a bridge between Garmin inReach satellite communicators and New Zealand's top alpine weather and avalanche APIs. 

Designed specifically for mountaineering in the Southern Alps, this bot intercepts location pings from your Garmin device, fetches hyper-local topographical weather data, compresses it into a highly efficient string (<160 characters), and fires it back to your device over the Iridium satellite network.

## 🚀 Features
* **Serverless & Stateless:** Runs entirely on Netlify Scheduled Functions (AWS Lambda) with practically zero hosting costs.
* **Edge State Management:** Uses Turso (libsql/SQLite) at the edge to remember your last known coordinates and Garmin session tokens.
* **Topographical Precision:** Uses `yr.no` to model exact temperatures and precipitation at your specific GPS altitude.
* **National Park Geofencing:** Automatically maps your GPS coordinates to the correct MetService National Park forecast and NZ Avalanche Advisory (NZAA) region.
* **Text Compression:** Uses a custom dictionary to squish qualitative weather text into a satellite-friendly format to avoid multi-message billing.
* **Routine Broadcasts:** Automatically pushes fresh MetService updates to your tent at 07:00 and 19:00 NZST.

---

## ⚙️ How It Works (The Architecture)

1. **The Ping:** You send a message or share your location from your Garmin inReach to a dedicated Gmail address.
2. **The Poller:** Every minute, a Netlify Cron job triggers the Go application.
3. **The Parser:** The bot connects to Gmail via IMAP, reads the Garmin email body, extracts your exact `Lat/Lon`, and saves your Garmin `extId` and `guid` session tokens to the Turso database.
4. **The Trigger:** If your location has changed, if the weather data is older than 12 hours, or if it is the 07:00/19:00 broadcast window, the bot initiates a fetch.
5. **The Fetch:** Using Go routines, it concurrently scrapes:
   * **yr.no:** For 48-hour quantitative precipitation and current temp at your exact altitude.
   * **MetService:** For the 48-hour qualitative summary and 3-tier vertical wind profile.
   * **Avalanche.net.nz:** For the current regional avalanche danger rating.
6. **The Delivery:** The bot combines, compresses, and trims the data to fit under 160 characters, then POSTs it directly to the Garmin Explore gateway. It arrives on your device seconds later.

---

## 📡 The Payload: Expected Weather Reports

Because satellite data is expensive and limited to 160 characters per message, the bot uses extreme compression. Here is how to read the data on the mountain.

### Example 1: Incoming Winter Storm (Aoraki/Mt Cook)
**The Output:**
> `YR T:-14C D1:25mm D2:2mm | MS(AOR) D1 HvyRain->Snow W1k:30 2k:60 3k:95 | D2 Clear W1k:10 2k:25 3k:40 | AVL:4-HIGH`

**The Breakdown:**
* **`YR` (yr.no data at 3,151m):** Current temp is -14°C. Day 1 expects a massive 25mm of precipitation (heavy snow). Day 2 expects 2mm (clearing).
* **`MS(AOR)` (MetService Aoraki):**
  * **Day 1:** Heavy rain turning to snow. Winds at 1000m (30km/h), 2000m (60km/h), and 3000m (95km/h - impassable).
  * **Day 2:** Fine/Clear. Winds drop to 10, 25, and 40km/h respectively (good climbing window).
* **`AVL:4-HIGH`:** The NZ Avalanche Advisory rates the region as Level 4 (High Danger).

### Example 2: Spring Climbing Window (Arthur's Pass)
**The Output:**
> `YR T:-2C D1:0mm D2:5mm | MS(ART) D1 Clear W1k:10 2k:15 3k:25 | D2 PrtlyCldy w/ IsoShwrs W1k:20 2k:35 3k:50 | AVL:2-MODR`

**The Breakdown:**
* **`YR` (yr.no data at 2,000m):** Temp is -2°C. No precip today, 5mm tomorrow.
* **`MS(ART)` (MetService Arthur's Pass):**
  * **Day 1:** Clear skies. Highly manageable winds peaking at 25km/h at 3000m.
  * **Day 2:** Partly cloudy with isolated showers. Winds increasing to 50km/h at the summits.
* **`AVL:2-MODR`:** The NZ Avalanche Advisory rates the region as Level 2 (Moderate Danger).

---

## 📚 API Endpoints

The `weather-api` function serves a handful of HTTP endpoints. They're exposed at friendly paths via `netlify.toml` redirects (the underlying function path is `/.netlify/functions/weather-api/*`, and each also has a `/weather-api/<name>` alias).

| Method & Path | Purpose | Auth | Notes |
|---|---|---|---|
| `GET /weather-api?lat=<lat>&lon=<lon>` | Compressed `<160`-char weather report for a coordinate (same payload the device receives). | — | `lat`/`lon` required & range-checked; returns `text/plain`. |
| `GET /weather-api/all` | The report for **every** registered park/region. | — | Handy for eyeballing all geofences at once. |
| `GET /log` | Usage log (`request_log`) rendered as a readable text table — who asked for what, when, and where. | `LOGS_KEY` | Alias: `/weather-api/log`. |
| `GET /debug` | Per-invocation captured `log.*` output as **JSON**, for the live viewer. | `LOGS_KEY` | `?after=<id>` for incremental polling; returns the most recent ~300 lines otherwise. |
| `GET /pause?mins=<N>` | Suspend the bot's inbound processing for `N` minutes (so a fresh single-use Garmin shortlink isn't burned by the cron). | `LOGS_KEY` | Default `15`, capped `120`; **auto-resumes** when it lapses. Returns JSON `{paused, mins, resumes_at_nzt}`. |
| `GET /resume` | Clear the pause flag immediately. | `LOGS_KEY` | Returns `{"resumed":true}`. |
| `POST /garmin-parse-test` | Upload a saved Garmin message page (raw HTML body); returns a parse **PASS/FAIL** verdict with the extracted `Guid`/`MessageId`/coords. | `LOGS_KEY` | Parsed in memory only, never stored. See [Validating the Real Garmin Page Parser](#-validating-the-real-garmin-page-parser). |

**Static pages** (served from `public/`):

| Path | Purpose |
|---|---|
| `/` (`index.html`) | User guide. |
| `/debug.html` | Live, colour-coded, auto-scrolling log viewer (polls `/debug`). |
| `/garmin-test.html` | Upload UI for the Garmin page parse test (calls `/garmin-parse-test`). |

Architecture & flow diagrams (Mermaid) live in [`docs/architecture.html`](docs/architecture.html) — open it locally in a browser; it is not deployed.

**Auth:** endpoints marked `LOGS_KEY` require `?key=<LOGS_KEY>` when the `LOGS_KEY` env var is set; if it's unset, they're open. Example:

```bash
curl "https://<your-site>/log?key=$LOGS_KEY"
curl "https://<your-site>/debug?key=$LOGS_KEY&after=0"
curl "https://<your-site>/pause?key=$LOGS_KEY&mins=15"
```

---

## 🧪 Testing Locally (The Email Loop)

You don't need to burn expensive Garmin satellite credits to test the bot. The application includes a built-in standard email testing loop.

1. Ensure your `.env` file is populated with your Turso DB credentials and your Gmail App Password. If your `session_state` table was created before routine broadcast deduplication, apply:
   ```sql
   ALTER TABLE session_state ADD COLUMN last_routine_nz TEXT DEFAULT '';
   ```
   (The bot falls back if the column is missing, but then 07:00/19:00 broadcasts may fire more than once per window.)
2. Run the application locally (single poll: loads env, connects to IMAP/Turso, runs one handler cycle). Production scheduled runs use the Lambda entrypoint; locally set `LOCAL_WEATHER_BOT=1` so the binary invokes the handler directly instead of waiting on the Lambda runtime API:
   ```bash
   ./scripts/build.sh
   export $(grep -v '^#' .env | xargs) && LOCAL_WEATHER_BOT=1 ./functions/weather-bot
   ```

---

## 🔬 Live Debug Logs & Tracing

Because the bot runs as a short-lived serverless function, you can't tail its stdout. Instead, every `log.*` line from each invocation is captured and persisted to a `debug_log` table in Turso, then streamed to a live web viewer.

* **Live viewer:** open **`/debug.html`** (e.g. `https://<your-site>/debug.html?key=<LOGS_KEY>`). It polls `/debug` every 2 s and renders a colour-coded, auto-scrolling, filterable log stream grouped by invocation. Levels (info / ok / warn / error) are inferred from the line.
* **JSON API:** `GET /debug?after=<id>&key=<LOGS_KEY>` returns log rows as JSON for incremental polling.
* **Auth:** set the `LOGS_KEY` env var to require `?key=` on both `/log` and `/debug` (leave unset for open access).
* **Retention:** the bot prunes `debug_log` rows older than 2 days on each run.

The pipeline is now traced end-to-end: sender routing, command/coordinate parsing, Garmin session handshake (redirect URL, CSRF token), and — crucially — the **Garmin POST response body**, so a `200`-with-error-page (CSRF/WAF rejection) is flagged instead of silently logged as "✅ sent".

### Dry-run mode (test the full path without a device)

Set `GARMIN_DRY_RUN=1` to exercise the entire receive → parse → build → send pipeline **without** POSTing to Garmin or burning satellite credits. Instead of hitting `explore.garmin.com`, the bot emails the exact payload it *would* have sent to `GARMIN_DRY_RUN_REPLY_TO` (falling back to `EMAIL_USER`):

```bash
export $(grep -v '^#' .env | xargs)
GARMIN_DRY_RUN=1 GARMIN_DRY_RUN_REPLY_TO=you@example.com LOCAL_WEATHER_BOT=1 ./functions/weather-bot
```

### Fast unit tests (no network / no IMAP)

```bash
go test ./cmd/bot/        # parsing + routing decision logic (parse_test.go)
go test -tags integration ./cmd/bot/   # full live email loop (needs Gmail + Turso creds)
```

### Local log viewer

```bash
LOCAL_WEATHER_API=1 ./functions/weather-api   # serves :9090 + public/ → http://localhost:9090/debug.html
```

---

## 🧭 Validating the Real Garmin Page Parser

The most fragile part of the bridge is scraping the live `explore.garmin.com` message page for the `Guid`/`MessageId` reply target and the sender's coordinates. Garmin changes that page from time to time, which silently breaks replies. There are two complementary checks.

### 1. Automated tests (committed real page)

```bash
go test ./cmd/bot/          # includes TestGarminReplyLoop_* — drives shortlink GET →
                            # redirect → page parse → JSON "Send" POST against a fake
                            # Garmin (httptest) using a real captured page fixture
go test ./internal/garmin/  # the shared page parsers in isolation
```

The parsers live in **`internal/garmin`** (`ParseReplyFields`, `ParseShortlinkCoords`, `AnalyzePage`) and are shared by both the bot and the upload endpoint — so the tests exercise the exact code the bot relies on.

### 2. On-demand check (upload a fresh page)

When you want to confirm the **current** live layout still parses — without committing a new fixture or rebuilding — capture a real page and upload it:

1. Open a fresh inReach message page in your browser and save it as HTML (DevTools → right-click the `<html>` element → **Copy outerHTML**, or **Save As**).
2. Run the parse test:
   * **Web UI:** open **`/garmin-test.html`** (e.g. `https://<your-site>/garmin-test.html`), enter your `LOGS_KEY`, pick the file, and click **Run parse test**.
   * **API:** `curl --data-binary @page.html "https://<your-site>/garmin-parse-test?key=<LOGS_KEY>"`
3. Read the verdict:
   * **PASS** (`ok: true`, Guid + MessageId found) → the bot can still reply to the device.
   * **FAIL** (e.g. *"Guid hidden input not found"*) → Garmin changed the page; fix the parser in `internal/garmin` before it breaks live replies.

Both paths run `internal/garmin.AnalyzePage`, so a PASS is a real guarantee. Coordinates are **optional** in the verdict — app-relayed messages legitimately report no GPS fix (`0,0`), and the bot falls back to the last-known location. The uploaded HTML is parsed in memory only and never stored; the endpoint is guarded by `LOGS_KEY` (like `/debug`).

> **Tip:** to capture a *fresh* page, suspend the bot first (`GET /pause?key=<LOGS_KEY>&mins=15`) so the minute-cron doesn't consume the single-use shortlink before you grab it, then `GET /resume?key=<LOGS_KEY>` when done.
