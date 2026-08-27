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

Each test file is its own binary and calls `Stack::start()` itself, which
runs `docker compose up -d --build` and brings the whole stack down again
(`docker compose down -v`) once that test finishes -- there's no sharing or
caching across tests, so expect each one to take a while.

Each test file defines exactly one test. Cargo runs tests within a single
file concurrently, but `Stack::start()` always targets the same fixed ports
and the same docker-compose project name. So two tests running in parallel
would collide with each other.

## Running

Requires Docker (and `docker compose` or `docker-compose`).

```sh
cd e2e-tests
cargo test
```
