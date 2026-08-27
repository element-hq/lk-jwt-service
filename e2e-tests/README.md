# End-to-end test suite

End-to-end test suite for lk-jwt-service. The suite drives the service
integrated as a Matrix application service against a real Synapse instance
and a real LiveKit SFU, all running as Docker containers.

The stack is duplicated: a second, independent Synapse + lk-jwt-service +
LiveKit trio runs alongside the first, so federated scenarios can be
exercised, too.

```
   e2e test binary (`cargo test`, driven from the host)
        │
        │ HTTP -- Synapse C-S API & lk-jwt-service directly
        │ WebSocket -- LiveKit SFU
        │
        ▼
 ┌──────────────────────────────────────────────────────────────────────────┐
 │                                                                          │
 │   stack 1                                                                │
 │   ┌──────────┐   AS/C-S API    ┌──────────────┐   Twirp   ┌──────────┐   │
 │   │ synapse  │◀───────────────▶│ jwt-service  │◀─────────▶│ livekit  │   │
 │   └────┬─────┘                 └──────────────┘           └──────────┘   │
 │        │                                                                 │
 │        │ S-S API                                                         │
 │        ▼                                                                 │
 │   stack 2                                                                │
 │   ┌──────────┐   AS/C-S API    ┌──────────────┐   Twirp   ┌──────────┐   │
 │   │ synapse2 │◀───────────────▶│ jwt-service2 │◀─────────▶│ livekit2 │   │
 │   └──────────┘                 └──────────────┘           └──────────┘   │
 │                                                                          │
 └──────────────────────────────────────────────────────────────────────────┘
```

All tests live in a single binary (`tests/e2e.rs`) and share one stack:
whichever test runs first calls `ensure_stack()`, which runs
`docker compose up -d --build` and waits for everything to become healthy;
every other test just waits on that same result. The stack is brought down
(`docker compose down -v`) once, after every test in the binary has
finished, regardless of whether Cargo ran them in parallel or serially.

Because the stack (and its Synapse instances) are shared across tests,
Matrix ID localparts are generated per-call with `unique_localpart()`
rather than hardcoded, so tests can register users concurrently without
colliding.

## Running

Requires Docker (and `docker compose` or `docker-compose`).

```sh
cd e2e-tests
cargo test
```
