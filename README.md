# AI Chatbot Backend

The standalone Go backend for the SKYMCM hotel assistant. It serves the AI
chat endpoint and the public hotel catalogue that
[ai-chatbot-frontend](https://github.com/Maynstayy/ai-chatbot-frontend) calls.

The chatbot code is transferred from MaynStay's `Hotel_Booking_Maynstay`
repository (branch `feature/ai-chatbot`):

- the Anthropic and Ollama providers
- the system prompt
- the deterministic router, domain rules, third-party and support replies
- sanitizing
- conversation context
- rate limiting
- the read-only hotel, room-availability and landmark tools

It runs without MaynStay. There's no login, booking, payment or vendor code,
and it never uses the MaynStay database.

## Quick start: local Ollama, no database

```bash
# 1. Ollama (https://ollama.com), with the default model
ollama pull qwen2.5:1.5b
ollama serve                # if it isn't already running on :11434

# 2. Backend
cp .env.example .env        # the defaults use the file catalogue and Ollama
go run ./cmd/api            # http://localhost:8080

# 3. Try it
curl -s localhost:8080/readyz
curl -s -H "Content-Type: application/json" \
  -d '{"message":"Show me hotels in Varanasi"}' \
  localhost:8080/api/v1/chatbot/message
```

With the default settings (`CATALOG_SOURCE=file`), hotels come from
[testdata/catalog.json](testdata/catalog.json). That file holds six **fake**
"Test …" hotels in Varanasi, Agra and Vrindavan, with room types, rate plans
(one with a discount), sold-out dates and landmarks. No database is needed.

Good test questions:

- "Which hotel in Varanasi is closest to Dashashwamedh Ghat?" (landmarks, search)
- "Check rooms at Test Riverside Palace for 2 guests from 2027-01-10 to 2027-01-12" (availability, discount)
- "Is the Family Suite at Test Ganga View Stay free from 2027-01-10 to 2027-01-15?" (sold out)
- "Show my bookings" (sign-in reply: no booking access for guests)
- "Customer support" (support channels)
- "Write me a python script" (off-topic reply)

`qwen2.5:1.5b` is small and fast, but it sometimes skips a tool or misstates a
detail. For better answers, try a larger Ollama model (`AI_MODEL=qwen2.5:7b`
after `ollama pull qwen2.5:7b`), or `AI_PROVIDER=anthropic` with a
test-only `AI_API_KEY`.

### Connect the frontend

In `ai-chatbot-frontend/.env.local`, set
`NEXT_PUBLIC_CHATBOT_API_URL=http://localhost:8080`, then run `npm run dev`
there. `CORS_ALLOWED_ORIGINS` already includes `http://localhost:3000`.

## Commands

| Command | What it does |
| --- | --- |
| `go run ./cmd/api` | run the API |
| `go test ./...` | all unit and integration tests (no database, no network: the AI is a stand-in server) |
| `go vet ./...` | static checks |
| `go run ./cmd/dbtool -confirm=<DB_NAME> migrate` | create the catalogue tables in the chatbot's own DB |
| `go run ./cmd/dbtool -confirm=<DB_NAME> seed` | load the fake catalogue into an empty DB (refused in production) |

## Configuration

See [.env.example](.env.example). Key settings:

- `CATALOG_SOURCE`: where hotels come from.
  - `skymcm`: the **real** listings, read from the SKYMCM website's public API
    (`SKYMCM_API_URL`). Required when `APP_ENV=production`; the backend refuses
    to start in production with anything else.
  - `file` (default) or `mysql`: fake test hotels, for local development and
    tests only.
- `SKYMCM_API_URL` / `SKYMCM_ASSET_URL`: the website's API and where its
  `/uploads/...` photos are served. `https://skymcm.com` is the candidate for
  both and is **not verified yet**. `https` is required; `http` is allowed
  only for `localhost` outside production.
- `DB_*`: only for `mysql`. `DB_NAME` must be `ai_chatbot_<env>`.
  - Any other name is refused before connecting.
  - A database that contains MaynStay booking tables (`signup`, `bookings`,
    `payments`, …) is refused after connecting.
- `AI_PROVIDER`: `ollama` (default, `qwen2.5:1.5b`), `anthropic` (`claude-opus-5-5`,
  needs `AI_API_KEY`) or `off`.
  - `AI_TIMEOUT_SECONDS` defaults to 90 and `AI_MAX_TOKENS` to 1024 (MaynStay's
    defaults).
- `CORS_ALLOWED_ORIGINS`: required in production.
- `TRUSTED_PROXIES`: needed when a proxy on another machine forwards client
  IPs. Without it, every visitor shares the proxy's rate-limit bucket.
- `REDIS_URL`: optional. Use it to share conversations and rate limits across
  instances.

Secrets go only in `.env` or the server environment. Never commit them, and
never put them in the frontend.

## Real hotels from the SKYMCM website

With `CATALOG_SOURCE=skymcm`, `internal/skymcm` reads the website's own
public, read-only endpoints server-to-server, the same data the previous
in-process chatbot read through MaynStay's services:

| Endpoint | Used for |
| --- | --- |
| `GET /api/v1/hotels` | listings: names, photos, locations, amenities, ratings, "from" price with GST |
| `GET /api/v1/hotels/{id}` | one hotel, with check-in/check-out times |
| `GET /api/v1/room-types` | live availability and final prices for dates and guests |
| `GET /api/v1/landmarks` | "near X" questions |

- **Prices** are used exactly as returned. The website has already applied
  discounts, seasonal rates, GST and the room count a party needs, so the
  chatbot's own pricing step is not used for this source.
- **Listings:** inactive or deleted hotels are never shown.
- **Photos:** paths like `/uploads/...` become full URLs on `SKYMCM_ASSET_URL`.
- **Safety:**
  - Only those four GET paths can be called.
  - No cookies, login headers or guest data are sent, and redirects to
    another host are refused.
  - Every response is time-limited, size-capped and strictly parsed;
    upstream error text is never logged or returned.
- **Load:**
  - At most `SKYMCM_MAX_CONCURRENCY` (default 4) requests run at once, and
    identical concurrent requests are shared.
  - Listings are cached 60 seconds, availability 30 seconds and landmarks
    30 minutes (`SKYMCM_LANDMARK_CACHE_SECONDS`).
  - An HTTP 429 pauses requests for its `Retry-After` (at most 60 seconds).
  - During an outage or pause, cached listings and landmarks are served for
    up to 10 minutes past their cache time. Availability is never served stale.
- **Readiness:** `/readyz` shows the API's last known state (`unknown`, `ok`,
  `degraded` or `rate_limited`) without calling it.

## Using a database (local tests)

1. Create a disposable database and a new user, for example `ai_chatbot_test`
   and `ai_chatbot_test`. Give the user rights on that schema only.
2. Set `CATALOG_SOURCE=mysql` and the `DB_*` values in `.env`.
3. `go run ./cmd/dbtool -confirm=ai_chatbot_test migrate`, then `... seed`.
4. `go run ./cmd/api`. `/readyz` should show `"database":"up"`.

The schema is in [migrations/0001_create_catalog.sql](migrations/0001_create_catalog.sql).
It has only catalogue tables (`hotels`, `room_types`, `hotel_rooms`,
`room_rate_plans`, `room_holds`, `landmarks`), with MaynStay's column names
so the transferred SQL is unchanged. There are no user, booking or payment tables.

## Layout

```
cmd/api              server entry point
cmd/dbtool           guarded migrate/seed tool
internal/chatbot     the AI assistant (transferred from MaynStay)
internal/hotels      read-only hotel catalogue: model, MySQL repository, public handler
internal/roomtypes   room types and availability: guest fit, discounts, GST
internal/rooms       rate plan model and pricing
internal/landmarks   landmarks for "near X" questions
internal/catalog     the same repositories from a JSON file (fake test data)
internal/bookings    booking record type only (booking tools are unreachable without login)
internal/gst         GST slabs
internal/config      environment, validation, DB-name guard
internal/database    MySQL connection and foreign-schema guard
internal/httpx       client IP behind proxies
internal/app         wiring, CORS, request log, health checks, graceful shutdown
pkg/cache, pkg/utils Redis wrapper, JSON responses and safe errors (from MaynStay)
migrations/          the chatbot's own schema
testdata/            fake catalogue
docs/API.md          endpoint reference with JSON examples
```

## Security notes

- Public guest access only. Booking tools are never offered, because they need
  a signed-in user and this backend has no login.
- A guest can't choose a conversation id. An unknown id gets a fresh
  server-generated id (128-bit random), so another visitor's id can only be
  used by someone who already has it.
- Limits: 8 KB request body, 1000-character messages, per-IP rate limits.
- Timeouts: `ReadHeaderTimeout` and read timeouts, a write deadline extended
  only for AI replies, and context timeouts on AI and DB calls.
- Secure errors: JSON only, internal details never sent, panics become a
  logged 500.
- Logs carry route, intent, outcome and timings, never message text or keys.
- Shutdown is graceful on SIGINT/SIGTERM.
- Serve it behind HTTPS (a reverse proxy or the platform's TLS).

## Not in V1

- **Server-side chat history.** History stays in the browser. MaynStay's
  history code is kept in `internal/chatbot/history*.go` behind
  `//go:build ignore`. It needs a guest identity before it can be enabled.
- **Seasonal/festival rates.** Removed with MaynStay's `seasonalrates`;
  `rate_plan_stay_total` is never set.
- **Booking lookups.** These need SKYMCM sign-in integration.
