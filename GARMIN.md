# GARMIN.md — the inReach messaging gateway, end to end

How a satellite message from a Garmin inReach becomes a weather reply on the
device. This documents the **whole pipeline** and, in particular, the **reply
path** — the fragile part that we reverse-engineered from a real browser
capture (see "Reply API" below). Confirmed working end-to-end on 2026-06-10
(`git tag v1.0.0`).

```
 inReach device ──satellite──> Iridium ──> Garmin ──email──> Gmail inbox
       ▲                                                          │
       │                                                     IMAP poll (cron, 1/min)
       │                                                          ▼
       │                                                   weather-bot Lambda
       │                                                  parse → geofence → fetch
       │                                                          │
       └────── JSON POST to explore.garmin.com <───── build <160-char report
              (the "reply path", below)                  (yr.no / MetService / NZAA)
```

## 1. Inbound: device → email

A user types a command (e.g. `UPDATE`) on the inReach (or in the Garmin
Messenger / Explore app paired over Bluetooth) and sends it. Garmin relays it
as an **email** to the bot's Gmail inbox (`EMAIL_USER`), from
`no.reply.inreach@garmin.com`, subject `inReach message from <device>`.

The email is **single-part `text/plain; charset=us-ascii`, quoted-printable**.
Body = the typed command, then fixed boilerplate:

```
Update

View the location or send a reply to <device>:
https://inreachlink.com/<token>

<device> sent this message from: Lat -44.988456 Lon 168.904967

Do not reply directly to this message. ...
```

A captured real example is the golden fixture `cmd/bot/testdata/garmin_update.eml`.

### Gotchas observed
- **One send arrives as ~4 emails.** A single UPDATE produces ~4 inbound emails,
  each with a **different** `inreachlink.com` shortlink. Usually the first few
  shortlinks are already **HTTP 500 (expired/single-use)** and only one resolves.
  The bot fails cleanly on dead links and replies once from the live one.
- **Coordinates may be absent.** Messages composed in the app (no GPS fix) omit
  the `Lat .. Lon ..` body stamp **and** report `0,0` on the message page →
  the bot falls back to the last-known location.
- **You cannot reply by email** — `Do not reply directly to this message`. The
  only way back to the device is the web reply API (section 4).

## 2. Bot ingest & parse (`cmd/bot/main.go`)

The `weather-bot` Lambda runs every minute (Netlify cron `* * * * *`):

1. `connectTurso` + `ensureSchema`, `loadState` (last coords/park/session).
2. IMAP-poll Gmail for UNSEEN messages (drain `Fetch` fully before `Store`).
3. For each message:
   - `extractEmailBody` decodes MIME / quoted-printable → plain text.
   - `isGarminSender` routes Garmin vs. human "test" senders.
   - `parseGarminBody` → `garminCommand{Shortlink, Lat, Lon, HasCoords, Start/Stop/Update}`.
     **Commands are detected only in `userMessage(body)`** (text before the
     boilerplate) so device names / boilerplate can't trigger a false command.
4. Mark the message seen (left unread on SMTP/send failure for retry).

Pure parse/route helpers (`isGarminSender`, `parseTestCommand`,
`parseGarminBody`, `userMessage`, `splitForGarmin`, `parseShortlinkCoords`,
`parseGarminReplyFields`, `garminSendOK`, `looksLikeGarminError`) are
unit-tested in `cmd/bot/parse_test.go` — keep them pure (no I/O).

## 3. Location, geofence & report

- **Coordinates** come from the email body if present; otherwise the bot
  fetches the message page once (section 4) and recovers them from the page's
  `"Locations":[{"Latitude":..,"Longitude":..}]` JSON (`parseShortlinkCoords`).
  `0,0` = no fix → use last-known location.
- **Geofence**: `forecast.GetClosestPark(lat,lon)` maps coords to an alpine
  region; `GetElevation` gives altitude.
- **Report**: `forecast.BuildReport(lat,lon,alt,park)` builds a `<160` char
  payload from yr.no + MetService + NZ Avalanche Advisory, e.g.
  `YR T:8C D1:0mm D2:3mm | MS(AOR) D1 Clear … | AVL:2-MODR`.
  `splitForGarmin` chunks anything over 160 chars (prefers the ` | D2 ` boundary).

## 4. Reply path: bot → device (the reverse-engineered part)

> ⚠️ This is the fragile part and it has changed before. The current
> implementation was rebuilt from a **HAR capture of a real browser "Send"**.
> If replies stop working, re-capture a HAR (see "Re-capturing" below).

### The flow
1. **GET the shortlink** `https://inreachlink.com/<token>` with a browser
   User-Agent. It 302-redirects to a **regional** host, e.g.
   `https://aus.explore.garmin.com/textmessage/txtmsg?extId=<token>`.
   ⚠️ The host is **region-specific** (this account = `aus.`). Derive it from
   the landed URL — **do not hardcode** `explore.garmin.com`.
   The shortlink is **single-use**, so the page is fetched **once**
   (`fetchInReachPage`) and reused for both location and the reply.
2. **Scrape the reply target** from the page's hidden inputs
   (`parseGarminReplyFields`):
   ```html
   <input name="Guid"      type="hidden" value="08dec69f-...-bd7c58220000">
   <input name="MessageId" type="hidden" value="160707281">
   ```
3. **POST the reply** (`SendGarminReply`), once per chunk:
   ```
   POST https://<host>/TextMessage/TxtMsg
   Content-Type: application/json
   X-Requested-With: XMLHttpRequest
   Origin:  https://<host>
   Referer: https://<host>/textmessage/txtmsg?extId=<token>

   {"ReplyAddress":"<EMAIL_USER>",
    "ReplyMessage":"<chunk>",
    "Guid":"<guid>",
    "MessageId":"<messageId>"}
   ```
4. **Success is `{"Success":true}`** in the JSON response (`garminSendOK`).
   A bare HTTP 200 is **NOT** proof of delivery.

### Why this is simpler than the old code thought
- **No `__RequestVerificationToken` / CSRF token.** The old form-POST approach
  is dead.
- **No session cookie required.** The captured POST sent none; the unguessable
  `Guid`/`MessageId` pair authorises it. Works from Netlify's egress IP (no WAF
  block observed).

### Routine broadcasts (07:00 / 19:00 NZ)
No fresh shortlink exists for scheduled sends, so `InitGarminSessionFromState`
rebuilds a session purely from persisted `reply_host` + `ext_id` + `guid` +
`message_id` (no network — the POST needs no cookies). These are saved to
`session_state` on every inbound message.

### Key functions (`cmd/bot/main.go`)
| Function | Role |
|---|---|
| `fetchInReachPage` | GET shortlink once → `{HTML, FinalURL, Host, ExtID}` |
| `parseShortlinkCoords` | recover sender lat/lon from page `Locations[]` (delegates to `internal/garmin.ParseShortlinkCoords`) |
| `parseGarminReplyFields` | scrape `Guid` + `MessageId` hidden inputs (delegates to `internal/garmin.ParseReplyFields`) |
| `newGarminSessionFromPage` | build a `GarminSession` from a fetched page |
| `InitGarminSession` | shortlink → session (wrapper: fetch + build) |
| `InitGarminSessionFromState` | rebuild session from Turso state (no network) |
| `SendGarminReply` | chunk + JSON-POST to `/TextMessage/TxtMsg` |
| `garminSendOK` | confirm `{"Success":true}` |
| `sendToGarmin` | single choke point; honours `GARMIN_DRY_RUN` |

## 5. Dry run & testing without a device

`sendToGarmin` is the single delivery path. With **`GARMIN_DRY_RUN=1`** it skips
the Garmin POST and **emails the payload** to `GARMIN_DRY_RUN_REPLY_TO`
(fallback `EMAIL_USER`) — exercising receive→parse→geofence→build→session
without a real device or satellite credits. `fetchInReachPage` still runs in
dry-run (a harmless GET), so the page fetch + `Guid`/`MessageId` scrape are still
validated. Set `GARMIN_DRY_RUN=0` (or unset) for real delivery.

Unit tests (no network): `go test ./cmd/bot/`. This now also includes
`TestGarminReplyLoop_*` (`cmd/bot/garmin_reply_loop_test.go`), which drives the whole
reply loop — shortlink GET → redirect → page parse (Guid/MessageId + sender coords) →
JSON "Send" POST — against a fake Garmin (`httptest`) server using the golden page
fixture `cmd/bot/testdata/garmin_message_page.html`. It covers the HTTP mechanics
(host/extId derivation, POST URL/headers/JSON body, chunking, success/failure detection)
deterministically, with no device.

**Live handshake (real Garmin, GET only — no send, no credits).** `TestGarminLiveHandshake`
(integration tag) validates the shortlink→page→Guid scrape against production when given
a fresh shortlink:
```
GARMIN_TEST_SHORTLINK="https://inreachlink.com/<token>" \
  go test -tags integration ./cmd/bot/ -run GarminLiveHandshake -v
```
Because the cron would burn the single-use link first, **suspend processing** while you
send the device message and grab the link (see §9).

**On-demand parse test (upload a real page).** To confirm the live Garmin layout still
parses without committing/rebuilding, save a real message page as HTML and upload it:

- Web UI: `https://acr-wx.netlify.app/garmin-test.html` — pick the saved `.html`, enter
  `LOGS_KEY`, **Run parse test** → PASS/FAIL with the extracted `Guid`/`MessageId`/coords.
- API: `curl --data-binary @page.html "https://acr-wx.netlify.app/garmin-parse-test?key=<LOGS_KEY>"`

Both call `internal/garmin.AnalyzePage`, the **same parser the bot uses**
(`ParseReplyFields` / `ParseShortlinkCoords`, shared by `cmd/bot` and `cmd/weather-api`),
so a PASS means the bot can still reply and a FAIL (e.g. "Guid hidden input not found")
means Garmin changed the page. The uploaded HTML is parsed in memory only — never stored.
`ok` is true when Guid+MessageId are found; coords are optional (app messages report no
fix). Unit-tested in `internal/garmin/garmin_test.go`.

## 6. Observability — watching it live

The bot is serverless; you cannot tail stdout. Every `log.*` line is captured
to the Turso `debug_log` table per invocation and streamed to a live viewer.

- **Live log viewer**: `https://acr-wx.netlify.app/debug.html?key=<LOGS_KEY>`
- **Raw JSON feed**: `https://acr-wx.netlify.app/debug?key=<LOGS_KEY>`
  (supports `&after=<id>` for incremental polling)
- **Usage log**: `https://acr-wx.netlify.app/log?key=<LOGS_KEY>`

Trace markers to look for in a healthy reply run:
```
🧭 Routing: … isGarmin=true …
🧭 Garmin parse: shortlink=true … update=true
🔗 inReach page: host=aus.explore.garmin.com extId=… (N bytes)
✅ Garmin session established (host=… extId=… msgId=…)
🧮 Built report (N chars): YR T:… | AVL:…
📨 Garmin POST part 1/1 → HTTP 200 (16 bytes resp)
✅ Sent to Garmin: part 1/1
```

## 7. Re-capturing the reply API (when it breaks again)

curl can't drive the JS, so capture a real browser Send:
1. Open a **fresh** (unopened) `inreachlink.com` shortlink in Chrome/Edge.
2. DevTools → **Network**, tick **Preserve log**, before loading the page.
3. Type a reply, click **Send**, watch for the `TxtMsg` POST.
4. Right-click → **Save all as HAR with content**.
5. The POST's URL, headers, and JSON body are the spec. ⚠️ A HAR holds session
   data — `*.har` is gitignored; never commit/share it publicly.
6. While you have the page open, **save its HTML** (DevTools → right-click the
   `<html>` element → Copy → Copy outerHTML, or "Save as") to
   `cmd/bot/testdata/garmin_message_page.html` as the offline test's golden fixture.
   **Sanitize before committing**: replace the real `Guid`/`MessageId` values with the
   test markers (`TEST-guid-0000-0000-000000000001` / `999000111`) and the `Locations[]`
   coords with `-43.730000 / 170.090000`, so no live session token is committed.

## 8. State & env

- **Turso** (`session_state` row `id='garmin_primary'`): `ext_id`, `guid`,
  `message_id`, `reply_host`, `active`, `lat/lon/alt`, `park`, `last_fetch`,
  `last_routine_nz`. Also `request_log` (usage) and `debug_log` (live trace).
- **Env**: `TURSO_DB_URL`, `TURSO_AUTH_TOKEN`, `EMAIL_USER`, `EMAIL_PASS`
  (Gmail **app password**, requires 2FA). Optional: `LOGS_KEY` (protects
  `/log` + `/debug` + `/pause` + `/resume`), `GARMIN_DRY_RUN`,
  `GARMIN_DRY_RUN_REPLY_TO`. Test-only: `GARMIN_TEST_SHORTLINK` (live handshake test).

## 9. Suspending processing (for live testing)

To capture a **fresh, single-use** inReach message without the minute-cron consuming it
(following — and burning — its shortlink and replying), suspend the bot first:

- **Pause**: `GET /pause?key=<LOGS_KEY>&mins=<N>` (default 15, capped 120). Sets
  `session_state.paused_until = now + N·60`. The bot checks this at the top of every
  cycle (`pausedUntil`) and, while active, **returns early before the IMAP poll** — so
  inbound mail is left UNSEEN and nothing is sent to any device. It **auto-resumes** when
  the timestamp lapses, so a forgotten pause can't silently drop real traffic.
- **Resume**: `GET /resume?key=<LOGS_KEY>` clears the flag (`paused_until = 0`).

```
curl "https://acr-wx.netlify.app/pause?key=<LOGS_KEY>&mins=15"
# send UPDATE from the inReach; copy a shortlink from the inbox email; run the live test
curl "https://acr-wx.netlify.app/resume?key=<LOGS_KEY>"
```

While paused, messages accumulate UNSEEN; on resume the bot processes them (mostly
failing on now-expired links). To avoid a late reply reaching the real device after a
test, mark those test emails read in Gmail before resuming.
