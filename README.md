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
