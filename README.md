# HMD Event Companion — API

Go backend for the House Moving Day guest app. It exists for one thing the
frontend cannot do on its own: give every guest a **personal invite link** that
the server issues, resolves and can revoke.

The whole thing runs with nothing installed and nothing registered.

```bash
go run ./cmd/server
# listening addr=:8080 store=memory cache=memory
```

With no `DATABASE_URL` it serves the same sample data the React app ships with
(32 guests, the rundown, the questions), from memory. With no `REDIS_URL` it
uses an in-process stand-in that honours the same contract. Add either URL
later and no code changes — see [Going live](#going-live).

---

## The invite workflow

This is the part worth testing first, because it is the one the prototype
cannot fake.

```
committee                     server                        guest
---------                     ------                        -----
POST /api/admin/guests  -->   mint 16-char token
                              store it against the guest
                        <--   https://app/u/budi-santoso-<token>

POST /api/admin/invites <--   fill the WhatsApp template
                        <--   wa.me link, ready to tap

                                        open /u/budi-santoso-<token>
                              GET /api/invite  <-- (token as bearer)
                        -->   { name, rsvp, checkedIn, code, cards, outfit }

                              POST /api/invite/rsvp
                              POST /api/invite/checkin
                              POST /api/invite/hadir  { hadir, outfit }
```

The name in front of the token is decoration, the way a name on a paper
invitation is: the server never reads it, and the token alone says who the
link belongs to. It is there because this URL is the first thing the guest
sees of the event, in WhatsApp. Splitting it stays unambiguous because a token
is always the last 16 characters — the alphabet has no dash in it.

`outfit` on the last two lines is what makes the character the guest's rather
than the browser's. It is stored against the guest, so opening the same link
on a second phone brings the same character back instead of an empty
customisation screen. It travels with `hadir` because it is the same event:
confirming the character is what makes a guest "Hadir".

### Why the token is the credential

Whoever opens the link is treated as that guest — the same model as a wedding
RSVP link or a shared calendar link. For a one-day event with a hundred guests
and no accounts, that is the right trade: nobody is going to create a password
to confirm they are coming to a party.

It is only safe because of three things, all of which are implemented:

- **16 characters of base32** from `crypto/rand` — 80 bits. Not guessable.
  The alphabet drops `i`, `l`, `o` and `u`, so a guest reading the link off a
  screenshot cannot mistype it into somebody else's.
- **Rate limited per IP.** 20 lookups a minute, so the keyspace cannot be
  walked. Token *shape* is checked before the limiter, so junk costs nothing
  and does not spend a real guest's budget.
- **Revocable.** `DELETE /api/admin/guests/{id}/invite` kills the link and
  returns `410 Gone` — not `404`, because the committee turning a link off is
  different information from a link that never existed. The guest row survives,
  so their check-in, cards and questions are not lost with it.

The token never travels in an API path. Paths get logged; this one is a
credential. It goes in `Authorization: Bearer`, or `?t=` where a header is
impossible (service worker prefetch, `<img>`), and query strings are not
logged.

> The prototype derives the link from the guest's name
> (`inviteLink()` in `src/admin/invite.ts`), so anyone can guess anyone else's
> by typing their name. That is fine for a mockup and not fine for a list of
> real phone numbers, which is why the token is minted server-side here.

---

## What Redis is actually for

"It helps when the internet is slow" is only half true, and worth being
straight about: **a guest's weak connection is handled on the client**, by
react-query's IndexedDB cache and the service worker. That already works and
Redis does not change it.

Redis earns its place on the server side, for four jobs:

| Job | Why it matters here |
|---|---|
| **Hot reads** | The rundown and guest list are read by every phone in the room and change a few times all day. From Redis that is ~1 ms instead of a round trip to Supabase, which on a free tier may sit in another country. |
| **Idempotency** | The frontend queues mutations offline and replays them. `Idempotency-Key` + Redis means the replay returns the *first* response instead of creating a second question, a second RSVP, a second card. |
| **Stage fan-out** | MC and speaker share one live state. In the prototype that is `localStorage`, which only syncs tabs in one browser. Pub/Sub makes it work between two actual phones. |
| **Rate limiting** | The invite lookup, the admin PIN and the stage code. |

Without Redis the in-process stand-in does all four, correctly, for **one**
instance. The moment there are two, they stop seeing each other's stage events
and keep separate rate-limit counters.

### Idempotency, concretely

```bash
# the same offline-queued write, replayed three times
for i in 1 2 3; do
  curl -X POST localhost:8080/api/questions \
    -H "Authorization: Bearer $TOKEN" \
    -H "Idempotency-Key: 7f3c1b10-aaaa-bbbb-cccc-000000000001" \
    -d '{"eventId":"talkshow","text":"Rencana 5 tahun ke depan?"}'
done
# -> one question. Calls 2 and 3 come back with `Idempotent-Replay: true`
#    and the identical body.
```

Send one key per logical write. A request without a key runs normally, so the
client decides what is worth protecting.

---

## Live MC ↔ speaker sync

`GET /api/stage/stream` is Server-Sent Events. MC, speaker and the committee's
notification bell all open the same stream.

SSE rather than WebSockets on purpose: the traffic is almost entirely one way,
it is plain HTTP so it survives venue proxies that break WebSocket upgrades,
and the browser reconnects by itself after a drop — which on event wifi is the
normal case, not the exception.

Writes to the shared state are **optimistically locked**. You send the `rev`
you read; if the other side wrote in between you get `409` with the current
`rev`, so neither phone can silently clobber the other mid-session.

```bash
curl -N localhost:8080/api/stage/stream -H "Authorization: Bearer $STAGE_TOKEN"
# retry: 2000
# data: {"kind":"state","rev":1,"state":{...}}
# data: {"kind":"question","question":{...}}
# : ping
```

The state document itself is stored opaquely. Its shape belongs to the
frontend (`src/stage/store.ts`) and changes as the stage sides grow;
re-declaring every field here would only create something to drift from.

---

## Going live

Both services have a free tier that is more than enough for this.

### 1. Supabase (Postgres)

1. Register at <https://supabase.com> → **New project**. Pick the
   **Southeast Asia (Singapore)** region — it is the closest to Jakarta and
   latency is the whole point.
2. Save the database password it shows you. It is not shown again.
3. **SQL Editor** → **New query** → paste all of
   [`migrations/001_init.sql`](migrations/001_init.sql) → **Run**.
4. Same again with [`migrations/002_lockdown.sql`](migrations/002_lockdown.sql).
   **Do not skip this one.** Supabase publishes every `public` table through
   PostgREST, and `guests` holds every guest's `invite_token` — their
   credential — plus their phone number. This app never uses that API, so the
   migration turns row level security on with no policies and closes it. The
   Go service is unaffected: it connects as the table owner, and RLS does not
   apply to the owner. You will see the globe icon disappear from each table
   in the Table Editor.
5. **Connect** → **ORMs** / **Postgres**. Copy the **pooled** string
   (port `6543`, host contains `pooler`), and add `sslmode=require`:

   ```
   DATABASE_URL=postgresql://postgres.abcdefgh:PASSWORD@aws-0-ap-southeast-1.pooler.supabase.com:6543/postgres?sslmode=require
   ```

   The pooled port, not `5432`: the free tier allows very few direct
   connections. The code sets `QueryExecModeDescribeExec` so it works on both.

   The dialog shows that string under an ORM tab. **Ignore everything else on
   it** — the `npm install prisma` steps, the `schema.prisma` tab, the agent
   skills. Prisma is a Node ORM; this service uses `pgx` and needs nothing but
   the URL.

   The Prisma string also carries `?pgbouncer=true`, which is a Prisma flag,
   not a Postgres one. Pasted into a pgx client it kills the connection with
   `FATAL: unrecognized configuration parameter "pgbouncer"`. Since that is
   the string the dashboard hands you, `sanitizeDSN` in
   `internal/store/postgres.go` strips it and the other client-only
   parameters, so the URL works exactly as copied.

On first start against an empty database the server loads the sample data by
itself, so it behaves exactly like memory mode and the workflow is immediately
testable.

You do **not** need the anon key or the service_role key. This service talks
to Postgres directly, not through PostgREST, so everything on the API Keys
page is irrelevant to it — the connection string is the only credential.

**Why Supabase over MongoDB.** This data is relational — guests ↔ cards ↔
questions ↔ feedback — and the admin dashboard is almost entirely counting and
grouping. In Postgres that is one aggregate query; in Mongo it is hand-written
pipelines doing the same joins in application code. Postgres also enforces the
rules that keep the numbers honest: `primary key (owner_id, from_id)` on
`cards` is what makes a replayed collect physically unable to double somebody's
prize count. Mongo's free tier would work; it would just be more code for a
weaker guarantee.

### 2. Redis

Two ways, and the choice barely matters because `REDIS_URL` is the whole
interface. Leave it empty and the API still runs — see "What Redis is actually
for" above for what you lose.

**Railway**, if the API is deployed there anyway: **New** → **Database** →
**Redis**. Railway puts `REDIS_URL` into the service's environment by itself,
so there is nothing to copy. The traffic stays inside Railway's private
network, which is both faster and one less password in a file.

**Upstash**, if you want Redis independent of where the API runs:

1. Register at <https://upstash.com> → **Create Database**.
2. Pick the region nearest your Supabase one.
3. **Connect** → copy the TCP / `redis-cli` URL. Note the **two** s —
   `rediss://` means TLS:

   ```
   REDIS_URL=rediss://default:PASSWORD@apn1-xxxx.upstash.io:6379
   ```

   Upstash bills per request, and the free tier is 10,000 a day. The cached
   reads here are short-TTL and shared across every phone, so a whole event
   day is a few thousand.

### 3. Fill in `.env`

```bash
cp .env.example .env
```

Then set `SESSION_SECRET` (`openssl rand -base64 32`), the two URLs, and
`APP_BASE_URL` — invite links are built from it, so it must match the origin
guests actually open. Testing on a phone means the LAN address, not
`localhost`:

```
APP_BASE_URL=http://192.168.18.31:5173
ALLOWED_ORIGINS=http://192.168.18.31:5173
```

### 4. Deploy to Railway

The `Dockerfile` is what Railway builds, and it is the same two stages you can
run locally with `docker build .`, so a build that works here works there.

1. <https://railway.app> → **New Project** → **Deploy from GitHub repo** →
   this repository.
2. **Variables**: paste `DATABASE_URL`, `SESSION_SECRET`, `APP_BASE_URL` and
   `ALLOWED_ORIGINS`, plus the four prototype codes if you changed them.
   Do **not** set `ADDR` or `PORT` — Railway injects `PORT` and the server
   binds it (`listenAddr` in `internal/config`). Setting `ADDR` overrides that
   and the health check then waits on a port nothing is listening to.
3. **New** → **Database** → **Redis** in the same project, which fills in
   `REDIS_URL` on its own.
4. **Settings** → **Networking** → **Generate Domain**.

Then point the two origin variables at the real addresses — they are what
guests' links are built from and what CORS allows, so they cannot stay
`localhost` once anything is deployed:

```
APP_BASE_URL=https://house-moving-day.vercel.app
ALLOWED_ORIGINS=https://house-moving-day.vercel.app
```

and on the frontend, `VITE_API_URL=https://<your-service>.up.railway.app`.

Check it came up:

```bash
curl https://<your-service>.up.railway.app/healthz
# {"ok":true,"store":"postgres","cache":"redis","mode":"persistent"}
```

`store` or `cache` reading `memory` means that service's variable did not
arrive. It is not a crash and the API will serve requests, which is exactly
why it is worth checking rather than assuming.

### Or test the real code paths locally first

```bash
docker compose up -d
# DATABASE_URL=postgresql://hmd:hmd@localhost:5432/hmd?sslmode=disable
# REDIS_URL=redis://localhost:6379
```

Same Postgres and Redis code paths, no signup. The migration runs on first
start. Note the compose healthcheck reports `healthy` slightly before
initialisation finishes — `docker compose logs postgres` and wait for
`database system is ready to accept connections` if a connection is refused.

---

## Wiring up the frontend

The React app is built for this: `src/data/adapters.ts` is the documented
single swap point, and nothing else in the app knows where data comes from.

Two changes are needed.

**1. A route for the link.** `/u/:token` stores the token and redirects into
the normal flow. Nothing else in `routing/` has to change.

**2. `adapters.ts` calls the API.** The function bodies change; the
components, queries and offline cache do not:

```ts
const API = import.meta.env.VITE_API_URL;
const tok = () => localStorage.getItem("hmd-invite-token") ?? "";

const req = (path: string, init?: RequestInit) =>
  fetch(`${API}${path}`, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${tok()}`,
      // One key per logical write, so the offline replay is safe.
      ...(init?.method && init.method !== "GET"
        ? { "Idempotency-Key": crypto.randomUUID() }
        : {}),
      ...init?.headers,
    },
  }).then((r) => (r.ok ? r.json() : Promise.reject(new Error(String(r.status)))));

export const api = {
  guests: () => req("/api/guests"),
  agenda: () => req("/api/agenda"),
  questions: () => req("/api/questions"),
  sendQuestion: (q: SendQuestion) => req("/api/questions", { method: "POST", body: JSON.stringify(q) }),
  // ...
};
```

One catch worth knowing before you start: `crypto.randomUUID()` per call is
wrong for a *retried* mutation — react-query retries the same mutation object,
and a fresh key each attempt defeats the whole point. The key has to be
generated when the mutation is **created** and reused across its retries.

Every JSON field the API returns already matches the TypeScript type it feeds,
so there is no mapping layer:

| API | TypeScript |
|---|---|
| `Guest` | `src/data/guests.ts` |
| `Session` | `src/data/agenda.ts`, `src/stage/data.ts` |
| `Question` | `AskedQuestion` in `src/data/questions.ts` |
| `AdminGuest` | `src/admin/data.ts` |

---

## API

Clock fields (`at`, `start`, `end`, `checkIn`) are **rundown minutes** —
minutes since midnight, Asia/Jakarta — which is the unit the whole app already
thinks in.

### Guest — `Authorization: Bearer <invite token>`

| | |
|---|---|
| `GET /api/invite` | Resolve the link: name, RSVP, check-in, kenalan code, card count |
| `POST /api/invite/rsvp` | `{"rsvp":"ya"\|"tidak"}` |
| `POST /api/invite/checkin` | `{"by":"qr"\|"kode"\|"panitia","code":"1108"}` |
| `POST /api/invite/hadir` | `{"hadir":true}` — character finished |
| `GET /api/guests` | Kenalan cards of everyone present |
| `GET /api/agenda` | Rundown |
| `GET /api/announcements` | |
| `GET /api/questions?eventId=` | `mine` is computed per caller |
| `POST /api/questions` | |
| `PATCH \| DELETE /api/questions/{id}` | Own questions only |
| `GET /api/cards` · `POST /api/cards` | `{"code":"1357","note":"..."}` |
| `POST /api/feedback/event` · `/final` | |

### Committee — email + PIN

| | |
|---|---|
| `POST /api/admin/login` · `/verify` | → session token |
| `GET \| POST /api/admin/guests` | List, or add one and mint their link |
| `POST /api/admin/invites` | `{"guestIds":[...]}` → wa.me links |
| `DELETE /api/admin/guests/{id}/invite` | Revoke |
| `GET /api/admin/dashboard` · `/feedback` | |
| `POST /api/admin/announcements` | |
| `PUT /api/admin/rundown` | |

### Stage — code `246810`

| | |
|---|---|
| `POST /api/stage/login` | `{"code","personId"}` → session token |
| `GET /api/stage/people` | |
| `GET \| PUT /api/stage/state` | `rev` is an optimistic lock |
| `PATCH /api/questions/{id}/status` | MC marks read / answered |
| `GET /api/stage/stream` | SSE — MC, speaker, admin |

`GET /healthz` says which store and cache are live, and whether anything is
actually being persisted.

---

## Layout

```
cmd/server/          main: config, wiring, graceful shutdown
internal/config/     env + .env, every knob with a working default
internal/model/      wire types; JSON tags match the TypeScript
internal/token/      invite tokens (crypto/rand) + signed sessions (HMAC)
internal/cache/      Cache interface -> Redis | in-process
internal/store/      Store interface -> Postgres | in-memory (seeded)
internal/httpapi/    routes, auth, idempotency, ETags, SSE
migrations/          001 schema, 002 lockdown, 003 outfit, 004 settings
Dockerfile           what Railway builds; same image locally
docker-compose.yml   local Postgres + Redis, for testing the real paths
```

Two interfaces carry the whole design. `Store` and `Cache` each have a real
implementation and a zero-setup one, which is why this runs before you have
registered anything, and why swapping in Supabase changes no HTTP code.

Authorisation lives in the store, not the handlers: `EditQuestion` takes the
caller's guest id and returns `ErrForbidden` itself. No handler can forget it.

---

## Still prototype

Honest list of what is deliberately not finished:

- **`POST /api/admin/login` returns the PIN in the response.** There is no mail
  service wired up. Shipping a second factor over the same channel defeats the
  point of having one — this field goes away when email works.
- **Invitations are not actually delivered.** The WhatsApp Business API needs
  an approved template and a paid number, so this hands back `wa.me` links the
  committee taps instead. The guest-facing half — a unique link, resolved
  server-side, landing on the RSVP screen — is complete and real.
- **Codes are shared secrets in env vars** (`STAGE_CODE`, `DOOR_CODE`,
  `PANITIA_PIN`, `ADMIN_PIN`). Fine for one event day, rate limited, and all
  of them must change before anything real.
- **No `read_count` on announcements yet.** The column exists; nothing reports
  reads.
- **Memory mode mints new invite tokens on every restart.** Links pasted into
  WhatsApp from a memory-mode run stop working when you restart. Use Postgres
  the moment you send a link to a real person.
