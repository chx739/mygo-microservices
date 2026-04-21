# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

A ride-sharing backend built with Go microservices: API Gateway, Trip Service, Driver Service, and Payment Service. Uses gRPC for synchronous calls, RabbitMQ for async events, MongoDB for persistence, and Stripe for payments.

## Commands

### Local Development (Kubernetes via Tilt)
```bash
tilt up          # Start all services with live reload
tilt down        # Stop all services
```

### Build Individual Services
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/api-gateway ./services/api-gateway
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/trip-service ./services/trip-service/cmd/main.go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/driver-service ./services/driver-service
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/payment-service ./services/payment-service/cmd/main.go
```

### Protobuf Generation
```bash
make generate-proto
```

### Observability
- Jaeger UI: `http://localhost:16686`
- RabbitMQ Management: `http://localhost:15672`

## Architecture

### Services and Communication

**Synchronous (gRPC):**
- API Gateway → Trip Service (port 9093): `PreviewTrip`, `CreateTrip`
- API Gateway → Driver Service (port 9092): `RegisterDriver`, `UnregisterDriver`

**Asynchronous (RabbitMQ topic exchange `trip`):**

| Routing Key | Publisher → Subscriber | Purpose |
|---|---|---|
| `trip.event.created` | Trip → Driver | Find available drivers |
| `trip.event.driver_assigned` | Trip → API Gateway | Notify rider via WebSocket |
| `trip.event.no_drivers_found` | Driver → Trip | No drivers available |
| `driver.cmd.trip_request` | Trip → Driver | Send trip request to driver |
| `driver.cmd.trip_accept/decline` | Driver → Trip | Driver response |
| `payment.cmd.create_session` | Trip → Payment | Initiate Stripe checkout |
| `payment.event.session_created` | Payment → Trip | Stripe session URL |
| `payment.event.success` | Payment → Trip | Payment confirmed |

### API Gateway Endpoints
- `POST /trip/preview` — calculate route and fare
- `POST /trip/start` — create trip
- `GET /ws/drivers` — WebSocket for driver updates
- `GET /ws/riders` — WebSocket for rider updates
- `POST /webhook/stripe` — Stripe payment webhook

### Shared Packages (`/shared/`)
- `messaging/` — RabbitMQ consumer/publisher with retry and DLQ support
- `db/` — MongoDB client init
- `tracing/` — OpenTelemetry + Jaeger setup
- `proto/` — Generated gRPC code
- `contracts/` — Routing key constants and message structs
- `env/` — Environment variable helpers

### Service Structure Pattern
Each service follows:
```
services/<name>/
├── cmd/main.go       # Entry point, wiring
├── internal/
│   ├── handler/      # gRPC handlers
│   ├── service/      # Business logic
│   ├── repository/   # Data access (MongoDB or in-memory)
│   ├── consumer/     # RabbitMQ message consumers
│   └── publisher/    # RabbitMQ message publishers
```

### Infrastructure
- `infra/development/k8s/` — Kubernetes manifests for local dev
- `infra/production/k8s/` — Kubernetes manifests for GCP
- `infra/development/docker/` — Dockerfiles (Alpine-based)

## Key Environment Variables

| Variable | Default | Used By |
|---|---|---|
| `RABBITMQ_URI` | `amqp://guest:guest@rabbitmq:5672/` | All services |
| `JAEGER_ENDPOINT` | `http://jaeger:14268/api/traces` | All services |
| `MONGODB_URI` | `mongodb://mongodb:27017` | Trip Service |
| `STRIPE_SECRET_KEY` | — | Payment Service |
| `HTTP_ADDR` | `:8081` | API Gateway |
| `STRIPE_WEBHOOK_KEY` | — | API Gateway |

# CLAUDE.md

Behavioral guidelines to reduce common LLM coding mistakes. Merge with project-specific instructions as needed.

**Tradeoff:** These guidelines bias toward caution over speed. For trivial tasks, use judgment.

## 1. Think Before Coding

**Don't assume. Don't hide confusion. Surface tradeoffs.**

Before implementing:
- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them - don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask.

## 2. Simplicity First

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.

Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

## 3. Surgical Changes

**Touch only what you must. Clean up only your own mess.**

When editing existing code:
- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code, mention it - don't delete it.

When your changes create orphans:
- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.

The test: Every changed line should trace directly to the user's request.

## 4. Goal-Driven Execution

**Define success criteria. Loop until verified.**

Transform tasks into verifiable goals:
- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"
- "Refactor X" → "Ensure tests pass before and after"

For multi-step tasks, state a brief plan:
```
1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]
```

Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.

---

**These guidelines are working if:** fewer unnecessary changes in diffs, fewer rewrites due to overcomplication, and clarifying questions come before implementation rather than after mistakes.
