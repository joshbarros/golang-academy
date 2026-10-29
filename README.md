# golang-academy

Course cohorts and the seats their enrollments take. The scarce number is the **seats left** in a cohort, and the **Cohorts** module is its only writer: it holds seats with a single conditional `UPDATE` that matches only while the seats left still cover the enrollment, so when two learners fight for the last seat, one commits a hold and the other is told the cohort is full and no hold is written. A learner reference that contains the poison token `POISON` is committed as rejected by the Passes module, the message is dead-lettered, and a compensating command gives the seats back. A later withdrawal also gives the seats back, but it does not roll back the access pass that was already issued.

The Go and Kubernetes version of [dotnet-academy](https://github.com/joshbarros/dotnet-academy), written in **Go** and run on
**Kubernetes**: the same saga, the same HTTP API and the same Angular UI, so the browser specs and the smoke script of the
.NET system run against this one unchanged. What is different is how it runs. **Kubernetes is what runs the stack**:
Postgres, Kafka, RabbitMQ, **two API replicas** and two web replicas, started in a local [kind](https://kind.sigs.k8s.io)
cluster with Kustomize manifests. Nothing is faked for the demo: the same pipeline (`make cluster-up`, the smoke script, the
browser specs, a script that kills pods mid-booking) runs in CI on a fresh kind cluster.

![The academy UI signed in as the administrator, after two enrollments fought for the last seats: one enrollment enrolled, one short](docs/screenshot.png)

*The UI reads everything from the API; shown signed in as the seeded administrator, on the cohort where two enrollments fought for the last seats: one enrollment was enrolled, the other was told it is short.*

## Enrollment statuses

```mermaid
flowchart LR
    Requested -->|seats held| Held
    Requested -->|short: no hold written| Rejected
    Held -->|access pass issued| Enrolled
    Held -->|poison reference: pass rejected| Rejected
    Held -->|withdrawal| Withdrawn
    Enrolled -->|withdrawal| Withdrawn
```

`Withdrawn` and `Rejected` (after a hold) give the seats back through a `ReleaseSeats` command, and Cohorts releases a hold at most once. The access pass has its own status (`Issued` or `Rejected`) and a withdrawal never changes it. If the withdrawal gets there before the final mark, the late mark finds the enrollment is no longer `Held` and does nothing.

## How the modules talk

```mermaid
sequenceDiagram
    participant C as Client
    participant E as Enrollments
    participant H as Cohorts
    participant P as Passes

    C->>E: POST /api/enrollments
    Note over E: enrollment + outbox row, one transaction
    E-->>C: 202 Requested
    E->>H: seats.reserve (relay → RabbitMQ, copy to Kafka)
    Note over H: inbox claim, conditional UPDATE, hold, outbox row
    alt seats left cover the enrollment
        H->>P: seats.held
        H->>E: seats.held (Requested → Held)
        alt reference is clean
            Note over P: access pass, outbox row
            P->>E: pass.issued (Held → Enrolled)
        else reference contains POISON
            Note over P: committed Rejected, then the message is dead-lettered
            P->>H: seats.release (compensating command)
            P->>E: pass.rejected (Held → Rejected)
            H->>E: seats.released
        end
    else cohort full
        H->>E: seats.short (Requested → Rejected, no hold)
    end
    C->>E: POST /api/enrollments/{id}/withdrawal
    E->>H: seats.release (Held or Enrolled → Withdrawn)
    H->>E: seats.released
```

| Module | Owns | Consumes | Emits |
| --- | --- | --- | --- |
| **identity** (`internal/identity`) | `identity` schema: users, roles, refresh tokens. | | |
| **enrollments** (`internal/enrollments`) | `enrollments.enrollments`: status, seats, fee, learner reference. Validation, the poison rule, the fee. | `seats.held`, `seats.short`, `pass.issued`, `pass.rejected`, `seats.released` | `seats.reserve`, `seats.release` (withdrawal) |
| **cohorts** (`internal/cohorts`) | `cohorts.cohorts.seats_left` (the only writer after the cohort opens) and `cohorts.seat_holds`. | `seats.reserve`, `seats.release` (prefetch 1) | `seats.held`, `seats.short`, `seats.released` |
| **passes** (`internal/passes`) | `passes.passes`. | `seats.held` | `pass.issued`, `pass.rejected`, `seats.release` (poison) |
| **platform** (`internal/platform`) | The relay, the consumer loop with retry and dead-letter, the Kafka log, problem+json and the middleware, config, migrations. Each module owns its own `outbox_messages` and `inbox_messages` tables. | | |

RabbitMQ topology is declared in code (`internal/platform/messaging/rabbit.go`): exchange `academy.work`, one queue per module
(`cohorts.commands`, `passes.work`, `enrollments.events`), a `.retry` queue per module (2 s TTL, then back to the main queue, 3
attempts), and a `.dlq` per module behind `academy.dlx`. A transient failure retries; a poison message goes straight to the
dead-letter queue after its rejection is committed. After a trip through the retry queue the broker's routing key is the queue
name, so the relay stores the original key in the message `type` and the consumer dispatches on that. Kafka has one topic,
`academy.events`, one partition, replication factor 1, keyed by enrollment id.

## Architecture

One binary (`cmd/api`), four modules in `internal/`. Each module has its own Postgres **schema** (`identity`, `enrollments`,
`cohorts`, `passes`), its own embedded SQL migrations and its own outbox and inbox tables. No module imports another; the one
cross-module read (a cohort's existence) goes through a small interface that the enrollments module declares and the cohorts module
satisfies, and `internal/app` is the only package that knows all of them.

```mermaid
flowchart TB
    main["cmd/api: serve | migrate"] --> app["internal/app: wiring, middleware chain, workers"]
    app --> identity & enrollments & cohorts & passes
    identity["internal/identity: users, roles, JWT, refresh tokens, two-factor"]
    enrollments["internal/enrollments: schema enrollments"]
    cohorts["internal/cohorts: schema cohorts, the seats and the holds"]
    passes["internal/passes: schema passes"]
    identity & enrollments & cohorts & passes --> platform["internal/platform: httpx, messaging, pgdb, config"]
    enrollments -. "Catalog.Find" .-> cohorts
```

Modules still talk only through messages: RabbitMQ carries the commands, Kafka keeps the event log, and the outbox and inbox
make delivery at-least-once and consumption idempotent. The dependencies are small on purpose: `pgx`, `amqp091-go`,
`franz-go`, `golang-jwt` and `x/crypto`; the HTTP layer is the standard library's `net/http` mux.

## On Kubernetes

```mermaid
flowchart LR
    host["127.0.0.1:8120 · :5240 · :15730"] -->|NodePort| web & api & mgmt
    subgraph ns["namespace academy (kind: one node)"]
        web["web ×2: nginx + Angular"] -->|/api| api
        api["api ×2: Go (init container: migrate)"] --> pg[("postgres StatefulSet")]
        api --> kafka[("kafka StatefulSet (KRaft)")]
        api --> rmq[("rabbitmq StatefulSet")]
        mgmt["RabbitMQ management"] --- rmq
    end
```

| What | How it is set up |
| --- | --- |
| API | A Deployment of **2 replicas**, `maxUnavailable: 0`, a PodDisruptionBudget (`minAvailable: 1`), spread over nodes. Nothing in it assumes a single instance: the relay claims outbox rows with `SKIP LOCKED`, the stock is taken with a conditional `UPDATE`, the auth rate limit and the session version live in Postgres. |
| Migrations | An **init container** runs `api migrate` before any replica serves (each module migrates behind a Postgres advisory lock, so several pods starting together are safe) and the API runs with `MIGRATE_ON_START=false`. |
| Probes | `startupProbe` and `livenessProbe` on `/api/health`; `readinessProbe` on `/api/health/ready`, which checks that every module's schema is there. |
| Data stores | One StatefulSet each for Postgres 17, Kafka 3.8 (KRaft, one node) and RabbitMQ 3.13, with a PVC each. Kafka and RabbitMQ keep their data across a pod restart. |
| Security | Pods run as non-root with a read-only root filesystem for the API, no privilege escalation, all capabilities dropped and the `RuntimeDefault` seccomp profile; the namespace enforces the `baseline` Pod Security level and warns on `restricted`. A NetworkPolicy lets only the API reach Postgres, Kafka and RabbitMQ (kind's `kindnet` enforces it). |
| Config and secrets | A ConfigMap (`academy-config`) and a Secret (`academy-secrets`). The kind overlay generates the demo Secret from `deploy/k8s/overlays/kind/secrets.env` (git-ignored, written by `scripts/kind-secrets.sh`, random signing key). A real cluster brings its own Secret under the same name. |
| Reaching it | `deploy/kind/cluster.yaml` publishes three NodePorts on `127.0.0.1` only: the web app on **8120**, the API on **5240**, RabbitMQ's management UI on **15730**. nginx reaches the API as `api.academy.svc.cluster.local` through the cluster DNS. |

The base manifests (`deploy/k8s/base`) carry the production defaults (email confirmation required, no dev mailbox, 20 auth
requests a minute). The kind overlay relaxes exactly what a demo needs: no email confirmation, the dev mailbox on (kept in
Postgres, so both replicas see the same messages), a delay on the final mark so the smoke script can land a refund inside the
window.

## Authentication and authorization

| Piece | How it works |
| --- | --- |
| Accounts | Passwords need 10+ characters with upper case, lower case and a digit, and 4 distinct characters, at most 128 (anything longer is refused before it is hashed). They are hashed with **argon2id** (19 MiB, 2 passes, per-hash salt). Five wrong passwords (or codes) lock the account for 15 minutes (`429`). A wrong password and an unknown email give the same `401`, and take the same time. |
| Email | Registration mails a confirmation link; with `AUTH_REQUIRE_CONFIRMED_EMAIL` on (the default) sign-in is refused (`403`) until it is opened. "Forgot password" mails a one-time reset link and answers `202` for every address, known or not. The links carry an HMAC token bound to the user, the purpose, an expiry and the user's *security stamp*, which rotates when the password changes, so each link works once. Messages go through a mailer interface: SMTP when `MAIL_HOST` is set, the log otherwise. |
| Two-factor | TOTP (RFC 6238, any authenticator app): `POST /api/auth/mfa/setup` returns the key and an `otpauth://` URI, `enable` proves it with a code and returns eight one-time recovery codes, `disable` needs the password and a code. Once on, sign-in needs a code (`401 A verification code is required.`). |
| Access token | JWT, 15 minutes, held **in memory** by the Angular app and sent as `Authorization: Bearer`. HS256 with a key of at least 32 bytes, or **RS256** when `JWT_RSA_PRIVATE_KEY_PEM` is set, in which case the public key is served at `/.well-known/jwks.json`. Only the configured algorithm is accepted (no `none`, no HS/RS confusion). |
| Revocation | Each token carries the user's session version. A password change, a reset, turning two-factor off and **"sign out everywhere"** bump it (one atomic `UPDATE`), and every token sold earlier stops working on every replica within `JWT_REVOCATION_CACHE_SECONDS` (default 5). |
| Refresh token | 384 random bits in an `HttpOnly`, `SameSite=Strict`, `Path=/api/auth` cookie (`Secure` over HTTPS). Only its SHA-256 is stored. Every refresh **rotates** it with a conditional `UPDATE`, so two concurrent refreshes with the same token cannot both win. Presenting a token that was already used revokes the whole family. Refresh and logout also need an `X-Requested-With: academy-web` header, so another site cannot trigger them. |
| Roles | `Customer` (default at registration), `Staff`, `Admin`. An administrator is seeded from `ADMIN_EMAIL` / `ADMIN_PASSWORD` on first start. |
| Rules | Anonymous: `/api/health*`, `/api/auth/register|login|refresh|logout` and the email flows. Every other route needs a token. Any signed-in user: read cohorts, book, read and withdraw **their own** enrollments and passes. `Staff` and `Admin`: open cohorts, read everything, read the event log and the dead letters. Another user's enrollment or pass is `404`, not `403`, so ids do not leak. The owner of a enrollment is the caller; a body field cannot set it. |
| Hardening | A rate limit on `/api/auth/*` per client (`RATE_LIMIT_AUTH_PER_MINUTE`), **counted in Postgres** so every replica shares one count, security headers on every response (`nosniff`, `DENY`, strict CSP on the API, `no-referrer`), a correlation id echoed in `X-Correlation-Id` and in every problem body, JSON logs (`LOG_JSON`), forwarded headers trusted only when `FORWARDED_HEADERS` is on, a panic handler, and a readiness endpoint that checks every module schema. |

## Run it

You need Go 1.26, Docker, [kind](https://kind.sigs.k8s.io), kubectl and Node 22 (for the Angular app and the browser specs).

```bash
make test-unit                  # no Docker
make deps                       # Postgres, Kafka and RabbitMQ in Docker, for `go run` and the integration tests
make test-integration           # a real Postgres: the database academy_test
make cluster-up                 # builds both images, creates the kind cluster, applies the manifests, waits until Ready
./scripts/smoke.sh              # last line: golang-academy smoke passed
npm ci && npx playwright install chromium
npx playwright test             # the browser specs against http://127.0.0.1:8120
./scripts/resilience.sh         # kills an API replica and RabbitMQ in the middle of a cohort (about two minutes)
make cluster-down
```

| Service | On your machine |
| --- | --- |
| Web (nginx, proxies `/api`) | <http://127.0.0.1:8120> |
| API (`/api/health`, `/api/health/ready`) | <http://127.0.0.1:5240> |
| RabbitMQ management (`platform` / `platform`) | <http://127.0.0.1:15730> |
| `make deps`: Postgres / Kafka / RabbitMQ | 5500 / 9150 / 5730 (management 15730, so stop the cluster first) |

Sign in as `admin@academy.local` / `AcademyAdmin2026` in the demo cluster, or register a customer.

## Try it with curl

The web app is at <http://127.0.0.1:8120> (`admin@academy.local` / `AcademyAdmin2026` in the demo stack, or register a customer). From the shell:

```bash
API=http://127.0.0.1:5240
login() { curl -s -X POST $API/api/auth/login -H 'content-type: application/json' -d "{\"email\":\"$1\",\"password\":\"$2\"}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["accessToken"])'; }
ADMIN="Authorization: Bearer $(login admin@academy.local AcademyAdmin2026)"

curl -s -X POST $API/api/auth/register -H 'content-type: application/json' \
  -d '{"email":"ana@example.com","password":"Passw0rd-Demo1"}'      # 201; its accessToken goes in $ANA below
ANA="Authorization: Bearer <accessToken>"

curl -s -H "$ADMIN" -X POST $API/api/cohorts \
  -H 'content-type: application/json' \
  -d '{"code":"DIST-SYS-101","title":"Intro to Distributed Systems","track":"Engineering","instructor":"Ada Lovelace","seatCapacity":10}'

curl -s -H "$ANA" -X POST $API/api/enrollments \
  -H 'content-type: application/json' \
  -d '{"cohortCode":"DIST-SYS-101","reference":"LRN-1001","seats":3}'
# 202, status Requested, feeCents 15200. Use the returned id:

curl -s -H "$ANA" $API/api/enrollments/<id>          # Enrolled after a few seconds, with a pass number
curl -s -H "$ANA" $API/api/passes/<id>
curl -s -H "$ANA" $API/api/cohorts/DIST-SYS-101      # seatsLeft 7
curl -s -H "$ADMIN" $API/api/enrollments/<id>/events   # the Kafka log for this enrollment

curl -s -H "$ANA" -X POST $API/api/enrollments/<id>/withdrawal \
  -H 'content-type: application/json'                       # seats come back; the access pass stays
```

An enrollment is validated at the door: learner reference required (at most 40 characters), 1 to 5 seats. The fee is 4,900 cents per seat plus a 500 cent platform fee. The poison match is a case-sensitive substring: `LRN-POISON-7` and any longer token containing `POISON` match.

## Develop

```bash
make deps && make run                 # the API on :5240 against the local dependencies (migrates itself)
npx nx serve web                      # http://localhost:4320, proxies /api to :5240
WEB_URL=http://localhost:4320 npx playwright test
make lint                             # gofmt and go vet
make logs                             # follow the API logs of every replica in the cluster
make cluster-status
```

## What is tested, and where it runs

- **Unit tests** (no Docker, under a second each): the enrollment rules and the fee (a fee per seat plus a flat charge, an overflow is an error, not a wrap) and the wording of the refusals, password hashing and policy, RFC 6238 test vectors for
  two-factor, access tokens (round trip, tampering, expiry inside and beyond the skew, `alg=none`, a token signed with another
  key, issuer and audience, RS256 and the algorithm confusion attack, the JWKS), one-time email tokens, the retry policy and
  the poison rule, the wire names of the messages and of the Kafka envelope, the problem+json responses, the correlation id and
  security headers, owner-or-staff access, config validation.
- **Integration tests** (`internal/integration`, a real Postgres, the real middleware chain and routes, brokers replaced by a
  loop that hands each outbox row to the handlers its routing key is bound to): register, login, lockout, refresh rotation,
  replay revoking the family, concurrent refreshes (exactly one wins), logout, change password, reset, email confirmation, the
  full two-factor lifecycle, the shared rate limit, RS256 and the JWKS, every route's 401/403/404 and ownership, paging,
  validation at the door; the cohorts module (**concurrent reservations never oversell**: sixty requests for thirty seats hold exactly thirty; a short reserve writes no hold and takes nothing; a release gives back exactly once; opening a cohort validates it) and the whole saga (enrollment to enrolled and a access pass, two requests fighting for the last seats where one commits and the other is short with no hold, poison dead-lettered and the seats released, a withdrawal after the sale that keeps the access pass, a late final mark that loses to a withdrawal, a repeated delivery that has no second effect, a final mark that arrives before the held mark and is retried); the outbox (several relays drain disjoint batches, a failed publish rolls the batch
  back, trimming keeps the inbox longer than the outbox) and migrations (six replicas migrating at once apply each file once,
  a broken file rolls back). Both suites run with the race detector in CI.
- **`scripts/smoke.sh`** drives the cluster over HTTP: authentication and ownership first (401, 403, 404), a password reset
  that ends the old sessions, validation, the happy path, the last seats with concurrent requests (one commits, the others are short and write no hold), the poison reference (`passes.work.dlq` gains one message, the seats restored), the dead letters listed and replayed, and a refund that lands between
  the document and the final mark.
- **Playwright** (`e2e/specs`, 14 specs, Chromium, against the web port of the cluster): sign-in and its refusals, the
  password policy, the session surviving a reload through the cookie, a password reset through the emailed link, two-factor
  sign-in, "sign out everywhere", the API refusing bad input and the page saying why, a cohort opened and enrolled in and withdrawn from in the UI, the last seats going to one enrollment while the other is told it is short, a poison enrollment that is rejected, the Kafka event log.
- **`scripts/resilience.sh`** is the one only Kubernetes makes possible: it kills an API replica and the RabbitMQ pod while three requests for two seats each compete for four, then checks that exactly two were enrolled, the third was told it is short, every seat is held and no more, exactly two passes exist, and that a withdrawal gives its seats back while the access pass stays.
- **CI** (`.github/workflows/ci.yml`) has two jobs. `verify`: gofmt and `go vet`, unit and integration tests with `-race`
  (Postgres 17 service container), `govulncheck`, `npm audit`, Prettier, the Angular type check, lint and build, and the
  Kustomize overlay rendering. `e2e`: creates a kind cluster, runs `make cluster-up`, the smoke script, the browser specs and
  the resilience script, and uploads cluster state, logs and traces on failure. Dependabot watches Go modules, npm, Actions and
  Docker.

## API

JSON over HTTP, the same as dotnet-academy's. Everything except health and `/api/auth/register|login|refresh|logout` and the
email flows needs a bearer token.

| Method and path | What it does | Responses |
| --- | --- | --- |
| `GET /api/health`, `GET /api/health/ready` | Liveness; readiness (every module schema reachable). | `200`, `503` |
| `POST /api/auth/register` | Creates a customer and mails a confirmation link. Signs them in unless confirmation is required. | `201` signed in, `202` check your inbox, `400` policy, `409` email taken, `429` |
| `POST /api/auth/login` | Signs in (with `code` when two-factor is on); sets the refresh cookie. | `200`, `401`, `403` unconfirmed, `429` locked or rate limited |
| `POST /api/auth/refresh`, `POST /api/auth/logout` | Rotate or revoke the refresh cookie. Need `X-Requested-With: academy-web`. | `200`/`204`, `400` header missing, `401` |
| `POST /api/auth/confirm-email`, `resend-confirmation` | Confirm an address with the emailed token; ask for a new link (always `202`). | `204`/`202`, `400` |
| `POST /api/auth/forgot-password`, `reset-password` | Mail a reset link (always `202`); set a new password with it (ends every session). | `202`/`204`, `400` |
| `POST /api/auth/change-password`, `logout-everywhere` | Signed in: change the password (ends the other sessions and returns a fresh one); end every session on every device. | `200`/`204`, `400`, `401` |
| `GET /api/auth/mfa`, `POST /api/auth/mfa/setup`, `enable`, `disable` | Two-factor status and lifecycle. | `200`/`204`, `400`, `409` |
| `GET /.well-known/jwks.json` | The public key set when RS256 is on. | `200`, `404` |
| `GET /api/auth/me` | The caller. | `200`, `401` |
| `POST /api/cohorts` | Opens a cohort with its seat capacity. Staff. | `201`, `400` invalid, `403`, `409` code exists |
| `GET /api/cohorts`, `GET /api/cohorts/{code}` | Cohorts with the live seats left. | `200`, `404` |
| `POST /api/enrollments` | Accepts an enrollment (1 to 5 seats) for the caller and starts the saga. | `202` Requested, `400` invalid, `404` unknown cohort, `409` duplicate reference |
| `GET /api/enrollments?cohortCode=&page=&pageSize=` | The caller's enrollments, newest first, a page at a time (`pageSize` at most 100, default 100). Staff: everyone's. The total is in `X-Total-Count`. | `200` |
| `GET /api/enrollments/{id}` | One enrollment and its status. Owner or staff. | `200`, `404` |
| `POST /api/enrollments/{id}/withdrawal` | Moves a held or enrolled enrollment to `Withdrawn` and gives the seats back. The access pass is not touched. Owner or staff. | `202`, `404`, `409` wrong status |
| `GET /api/enrollments/{id}/events` | The Kafka event log for one enrollment. Staff. | `200`, `403` |
| `GET /api/admin/dead-letters`, `POST /api/admin/dead-letters/{queue}/replay` | What waits in each dead-letter queue, with the first messages; publish them again. Staff. A message that still fails for the same reason is dead-lettered again. | `200`, `403`, `503` brokers off |
| `GET /api/passes/{enrollmentId}` | The access pass row. Owner or staff. | `200`, `404` |

Errors come back as `application/problem+json` with a `title` and the `correlationId`.

## Configuration

The process is configured from the environment (the ConfigMap and Secret in the cluster). Defaults work against `make deps`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://academy:academy@localhost:5500/academy?sslmode=disable` | The one Postgres database; each module has its own schema. |
| `RABBITMQ_URL` / `KAFKA_BROKERS` | `amqp://platform:platform@localhost:5730/` / `localhost:9150` | Work queue and event log. |
| `MESSAGING_ENABLED` | `true` | Switch the relay and the consumers off (the integration tests do). |
| `JWT_KEY` | none | HS256 signing key, at least 32 bytes. Required; the process refuses to start without it (or `JWT_RSA_PRIVATE_KEY_PEM`). |
| `JWT_ACCESS_MINUTES` / `JWT_REFRESH_DAYS` / `JWT_SECURE_COOKIES` | `15` / `14` / `false` | Token lifetimes; set `JWT_SECURE_COOKIES` when TLS ends in front of the API. |
| `JWT_RSA_PRIVATE_KEY_PEM` / `JWT_KEY_ID` | unset / `academy-1` | Sign with RS256 and publish the public key at `/.well-known/jwks.json`. |
| `JWT_REVOCATION_CACHE_SECONDS` | `5` (`2` in the demo) | How long a replica trusts a cached session version before re-reading it. |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | unset | The administrator created on first migration. |
| `AUTH_REQUIRE_CONFIRMED_EMAIL` / `AUTH_PUBLIC_URL` | `true` / `http://localhost:4320` | Sign-in waits for email confirmation; the address links in emails point to. |
| `MAIL_HOST` / `MAIL_PORT` / `MAIL_USER` / `MAIL_PASSWORD` / `MAIL_FROM` / `MAIL_START_TLS` | unset | SMTP for confirmation and reset emails. Unset means the log only. |
| `MAIL_DEV_MAILBOX` | `false` (`true` in the demo) | Keep the last messages in Postgres and serve them at `/api/dev/mailbox`. Never on in production. |
| `RATE_LIMIT_AUTH_PER_MINUTE` | `20` (`1000` in the demo) | Requests a minute per client to `/api/auth/*`. |
| `FORWARDED_HEADERS` | `false` (`true` behind nginx) | Trust `X-Forwarded-For` and `-Proto` from the reverse proxy. |
| `OUTBOX_RETENTION_DAYS` / `INBOX_RETENTION_DAYS` | `7` / `30` | Sent outbox rows and old inbox passes are deleted after this long. The inbox is kept longer because it must outlive any redelivery. |
| `ORDERS_FINAL_MARK_DELAY_MS` | `0` (`1500` in the demo) | Milliseconds the final mark waits. A latency knob so the smoke script can land a refund inside the window. |
| `MIGRATE_ON_START` | `true` | `false` in the cluster, where the init container migrates. |
| `LISTEN_ADDR` / `LOG_JSON` | `:8080` / `false` | |
| `TEST_DATABASE_URL` (tests only) | `postgres://academy:academy@localhost:5500/academy_test?sslmode=disable` | The Postgres the integration tests use; the database is created if it is missing. |

## Look inside and fix things

```bash
make cluster-status                                   # pods, services, disruption budgets
kubectl -n academy logs -f deploy/api --all-containers   # JSON logs (one line per request, one per worker event)
kubectl -n academy exec postgres-0 -- psql -U academy -d academy -c 'select routing_key, sent_at from enrollments.outbox_messages order by id desc limit 10'
kubectl -n academy delete pod -l app.kubernetes.io/name=api --wait=false   # a replica dies; the Service keeps answering
open http://127.0.0.1:15730                           # RabbitMQ management, platform / platform
```

- **Dead-lettered messages** sit in `passes.work.dlq` (and the two other `.dlq` queues). Staff can list and replay them from
  `/api/admin/dead-letters`, or inspect them in the management UI. Retries wait in `<queue>.retry` for 2 seconds.
- **A port is already in use**: `lsof -nP -i :8120`. The kind cluster and `make deps` both want RabbitMQ's management port
  (15730); stop one before starting the other.
- **Start clean**: `make cluster-down && make cluster-up` (the PVCs go with the cluster) and `make deps-down` for the
  dependencies.
- **A pod is not Ready after `make cluster-up`**: `kubectl -n academy describe pod <name>` and the logs of the `migrate` init
  container, which is where a wrong database password shows up first.

## Known limits

- It runs on **kind**, a one-node cluster on your machine. Nothing here is a cloud deployment: there is no ingress controller,
  no TLS, no autoscaler (the HPA needs a metrics server) and one broker of each kind. The manifests are plain Kubernetes and
  Kustomize, and the stateful services are the part you would replace with managed ones.
- The fee is a fixed amount per seat plus a flat charge: there is no price list, no discount and no tax. The system is about one thing, the
  scarce number, and what it costs to keep it honest.
- Revocation is eventual by design: a replica may honour a revoked access token for up to `JWT_REVOCATION_CACHE_SECONDS` seconds.
  Set it to `0` to check the database on every request.
- Delivery is at-least-once, not exactly-once. The inbox makes repeats harmless for the saga; the Kafka log can hold a
  duplicate event after a relay crash.
- `GET .../events` reads the Kafka topic for one key, so it scans the topic. Fine here, not something to expose at scale.
- Sign-in is email and password plus optional TOTP. There are no external identity providers and no WebAuthn passkeys.
- The browser specs drive Chromium at a desktop size only, and the `e2e` CI job needs a few minutes to build and start the cluster.

## Layout

```
cmd/api            the binary: `serve` and `migrate`
internal/app       wiring, the middleware chain, the background workers
internal/identity  users, roles, JWT, refresh tokens, two-factor, email flows      (schema identity)
internal/enrollments enrollment rules, the saga's first step and the event handler   (schema enrollments)
internal/cohorts     the cohort and its seats, the holds   (schema cohorts)
internal/passes      passes   (schema passes)
internal/platform  config, pgdb (pool, migrations), httpx, messaging (outbox, relay, consumers, Kafka log, dead letters)
internal/integration  the integration suite     internal/testutil  the shared test database
apps/web           Angular 21, styled with Tailwind 4 utilities only (shared with dotnet-academy)
e2e                Playwright specs
deploy/k8s         Kustomize base and the kind overlay       deploy/kind  the cluster     deploy/compose.deps.yaml  local dependencies
scripts            smoke.sh, resilience.sh, kind-secrets.sh
Dockerfile         two targets: api (distroless) and web (nginx)
```

## Other systems in this series

- [dotnet-academy](https://github.com/joshbarros/dotnet-academy): this system on ASP.NET Core and Docker Compose, with the same API and UI.
- [golang-sole](https://github.com/joshbarros/golang-sole): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-mint](https://github.com/joshbarros/golang-mint): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-pledge](https://github.com/joshbarros/golang-pledge): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-bistro](https://github.com/joshbarros/golang-bistro): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-flash](https://github.com/joshbarros/golang-flash): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-encore](https://github.com/joshbarros/golang-encore): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-atelier](https://github.com/joshbarros/golang-atelier): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-freight](https://github.com/joshbarros/golang-freight): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-clinic](https://github.com/joshbarros/golang-clinic): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-warehouse](https://github.com/joshbarros/golang-warehouse): the same kind of system for a different scarce number, also Go on Kubernetes.
- The `dotnet-*` series (freight, clinic, ledger, warehouse, academy, compute, hotel, tickets, carbon, charging, promo, mint, pledge, bistro, flash, encore, atelier, sole) has the whole family on ASP.NET Core; each adds one more way to keep a scarce number honest.
