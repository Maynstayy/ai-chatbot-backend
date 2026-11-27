# API reference

Base URL: the backend's origin, set in the frontend as `NEXT_PUBLIC_CHATBOT_API_URL`.
Every endpoint is public: there is no login, and no `Authorization` header is read.
Errors are always `{"error": "<message written for guests>"}`. Internal details
(SQL, network, stack traces) never reach the client. Each response carries an
`X-Request-ID` that matches the server log line.

| Method | Path | Used by the frontend for |
| --- | --- | --- |
| POST | `/api/v1/chatbot/message` | AI replies (`features/chat/api/assistant.ts`) |
| GET | `/api/v1/hotels` | destinations, guided search, hotel names (`lib/catalog.ts`) |
| GET | `/api/v1/hotels/{id}` | one hotel (not used by the chat UI today) |
| GET | `/api/v1/room-types` | live availability in the guided search (`lib/catalog.ts`) |
| GET | `/uploads/...` | hotel photos stored as paths (only when `UPLOADS_DIR` exists) |
| GET | `/healthz` | liveness |
| GET | `/readyz` | readiness (database ping when `CATALOG_SOURCE=mysql`) |

Chat history stays in the browser in V1, so there are no `/conversations` endpoints.

The hotel endpoints keep the same JSON whatever the catalogue source. With
`CATALOG_SOURCE=skymcm` they return the SKYMCM website's real listings,
availability and final prices, read from its public API. Photo paths become
full URLs and inactive hotels are left out. With `file` or `mysql` they
return the fake test hotels (local tests only).

## POST /api/v1/chatbot/message

The body is capped at 8 KB and `message` at 1000 characters. Rate limit:
`CHATBOT_RATE_LIMIT_PER_MINUTE` per visitor IP (default 20).

Request:

```json
{
  "message": "Which hotel in Varanasi is closest to Dashashwamedh Ghat?",
  "conversation_id": "",
  "context": { "destination": "Varanasi", "budget_min": 0, "budget_max": 3000 }
}
```

- `conversation_id`: send back the id from the previous reply to continue that
  conversation. Use empty or omit it to start a new one. An unknown or
  expired id (30 minutes idle) starts a fresh conversation with a new
  server-assigned id; a guest can't choose an id. `conversationId` is
  accepted too.
- `context` (optional): what the guided search already chose
  (`destination`, `check_in`, `check_out`, `guests`, `rooms`, `budget_min`,
  `budget_max`, `hotel_ids`). It's treated as data; malformed fields are
  dropped.
- `language` (optional, legacy): `auto`, `english`, `hindi` or `hinglish`.
  Replies are always in English.

`200 OK`. `message` is the reply text and `data` holds the structured results.
Hotel and room cards come only from catalogue lookups, never from model text.

```json
{
  "success": true,
  "message": "The closest hotel to Dashashwamedh Ghat is Test Riverside Palace. ...",
  "data": {
    "conversation_id": "d3ddbc4f6bc26385e3b20c647b26da88",
    "intent": "hotel_search",
    "requires_more_information": false,
    "requires_login": false,
    "hotels": [
      {
        "hotel_id": 103, "name": "Test Riverside Palace", "city": "Varanasi", "state": "Uttar Pradesh",
        "location": "Dashashwamedh", "category": "Luxury", "price_per_night": 8200, "tax_per_night": 1476,
        "rating": 4.8, "total_reviews": 210, "amenities": ["Free WiFi", "Pool", "Spa", "Breakfast", "Airport shuttle"],
        "free_cancellation": true, "pay_at_property": false,
        "thumbnail_url": "https://images.unsplash.com/...", "description": "TEST DATA - not a real property. ..."
      }
    ],
    "room_types": [],
    "bookings": [],
    "bookings_checked": false,
    "support": [],
    "search": { "destination": "Varanasi", "total_matches": 3 }
  }
}
```

A dated availability question fills `room_types`:

```json
{
  "hotel_id": 103, "room_type_id": 1031, "name": "Palace Room", "capacity": 2,
  "price_per_night": 8200, "tax_per_night": 1476, "available_count": 5,
  "rooms_needed": 1, "fits_guests": true, "check_in": "2027-01-10", "check_out": "2027-01-12", "guests": 2,
  "rate_plan_id": 10312, "rate_plan_name": "Winter offer",
  "rate_plan_price_per_night": 8100, "rate_plan_tax_per_night": 1458
}
```

- `intent` is one of `hotel_search`, `hotel_info`, `booking_help`, `booking_status`,
  `payment`, `cancellation_refund`, `travel`, `greeting`, `general` or
  `customer_support`.
- `support` holds verified contact channels on a customer-support reply:
  `{ "kind": "phone", "label": "Call Us", "href": "tel:+91...", "value": "...", "note": "..." }`.
- `bookings` is always empty and `requires_login` is `true` for a question about
  "my bookings". There is no login here, so booking lookups are never run.
- `tools_used` appears only with `CHATBOT_DEBUG=true` (refused in production).

Errors:

| Status | When |
| --- | --- |
| 400 | invalid JSON, empty message, message over 1000 characters, bad `language` |
| 413 | body over 8 KB |
| 429 | rate limit (`Retry-After: 60`) |
| 503 | AI not configured (`AI_PROVIDER=off`, or Anthropic without a key) or provider error |
| 504 | the AI took longer than `AI_TIMEOUT_SECONDS` |

## GET /api/v1/hotels

Query parameters (all optional): `city`, `search` (name or location), `category`,
`min_price`, `max_price`, `sort` (`price_asc`, `price_desc` or `rating`; newest
first by default). Only listed (active) hotels are returned. The response is
a bare JSON array:

```json
[
  {
    "hotel_id": 202, "name": "Test Agra Fort Lodge", "location": "Rakabganj", "description": "TEST DATA - ...",
    "city": "Agra", "state": "Uttar Pradesh", "country": "India", "address": "21 Test Marg, Agra",
    "latitude": 27.1795, "longitude": 78.0211, "category": "Budget",
    "price_per_night": 1100, "rating": 4, "total_reviews": 42, "amenities": ["Free WiFi", "Parking"],
    "thumbnail_url": "https://images.unsplash.com/...", "images": ["https://images.unsplash.com/..."],
    "owner_id": 0, "is_active": true, "is_deleted": false,
    "free_cancellation": false, "pay_at_property": false, "instant_booking": false,
    "child_policy": "...", "pet_policy": "...", "id_rules": "...",
    "gst_rate": 0.05, "tax_per_night": 55, "tax_rate": 0, "check_in_time": "14:00", "check_out_time": "11:00",
    "created_at": "2026-10-05T10:00:00Z", "updated_at": "2026-10-05T10:00:00Z"
  }
]
```

- `price_per_night` is the "from" price: the cheapest room type that has rooms.
- `tax_per_night` is the GST on that price: 0% up to ₹1,000, 5% up to ₹7,500, 18% above.

## GET /api/v1/hotels/{id}

Returns one hotel object as above, or `404 {"error":"hotel not found"}`.

## GET /api/v1/room-types?hotel_id=&check_in=&check_out=&guests=

- `hotel_id` is required.
- `check_in` and `check_out` (YYYY-MM-DD) must be given together. They scope
  `available_count` to the stay, minus overlapping holds.
- `guests` (1–200) adds `rooms_needed`, `fits_guests`, `combo_price_per_night`
  and `recommended`, and orders the list best-first.

The response is a bare JSON array:

```json
[
  {
    "room_type_id": 1011, "hotel_id": 101, "hotel_name": "Test Ganga View Stay", "name": "Deluxe Ganga View",
    "capacity": 2, "price_per_night": 3800, "description": "...", "amenities": ["Air conditioning", "Ganga view"],
    "images": [], "bed_type": "Queen", "size_sqm": 24, "view_type": "River",
    "total_rooms": 6, "available_count": 3, "gst_rate": 0.05, "tax_per_night": 190,
    "rooms_needed": 1, "fits_guests": true, "combo_price_per_night": 3800, "recommended": true,
    "rate_plans": [
      { "rate_plan_id": 10111, "room_id": 1011, "name": "Room only", "price_per_night": 3800, "max_occupancy": 2,
        "free_cancellation": true, "pay_at_property": true, "breakfast_included": false, "inclusions": [],
        "is_active": true, "discount_type": "none", "discount_value": 0, "gst_rate": 0.05, "tax_per_night": 190 }
    ]
  }
]
```

`400` for a bad date, a lone `check_in` or `check_out`, or bad `guests`.

## GET /healthz and GET /readyz

```json
{"status":"ok"}
{"status":"ok","catalog":"file","ai":"configured"}
```

With MySQL, `/readyz` adds `"database":"up"`. If the ping fails it answers 503
with `"database":"down"`.
