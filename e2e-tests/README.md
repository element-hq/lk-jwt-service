# End-to-end test suite

End-to-end test suite for lk-jwt-service. The suite drives the service
integrated as a Matrix application service against a real Synapse instance
and a real LiveKit SFU, all running as Docker containers.

The stack is duplicated: two independent Synapse + lk-jwt-service +
LiveKit trios, A and B, run alongside each other, so federated scenarios
can be exercised, too.

```
   e2e test binary (`cargo test`, driven from the host)
        │
        │ HTTP -- Synapse C-S API & lk-jwt-service directly
        │ WebSocket -- LiveKit SFU
        │
        ▼
 ┌──────────────────────────────────────────────────────────────────────────┐
 │                                                                          │
 │   stack A                                                                │
 │   ┌───────────┐  AS/C-S API   ┌───────────────┐   Twirp  ┌───────────┐   │
 │   │ synapse-a │◀─────────────▶│ jwt-service-a │◀────────▶│ livekit-a │   │
 │   └─────┬─────┘               └───────────────┘          └───────────┘   │
 │         │                                                                │
 │         │ S-S API                                                        │
 │         ▼                                                                │
 │   stack B                                                                │
 │   ┌───────────┐  AS/C-S API   ┌───────────────┐   Twirp  ┌───────────┐   │
 │   │ synapse-b │◀─────────────▶│ jwt-service-b │◀────────▶│ livekit-b │   │
 │   └───────────┘               └───────────────┘          └───────────┘   │
 │                                                                          │
 └──────────────────────────────────────────────────────────────────────────┘
```

Each test file is its own binary and defines exactly one test. They all
share a single stack: bringing it up costs minutes, so the tests never do it
themselves. `cargo xtask e2e` runs `docker compose up -d --build` once, waits
for every component to report healthy, runs the whole suite against that one
stack and finally tears it down again with `docker compose down -v`. A test
run that skips the task fails immediately with a pointer back to it, rather
than a wall of connection errors.

Because the homeservers outlive each individual test, Matrix ID localparts
are generated per registration with `unique_localpart()` rather than
hardcoded, so tests never collide on a name -- not even across repeated runs
against a stack kept alive with `--keep`.

## Running

Requires Docker (and `docker compose` or `docker-compose`).

```sh
cargo xtask e2e
```

Anything after `--` goes to `cargo test`, so a single test can be picked out
with

```sh
cargo xtask e2e -- get_token_local_sfu_succeeds
```

While iterating, `--keep` leaves the stack running afterwards so the next run
starts straight into the tests:

```sh
cargo xtask e2e --keep
cargo xtask e2e-down   # once you're done
```

`cargo xtask e2e-up` brings the stack up without running anything, which is
handy for poking at Synapse or the SFU by hand. Tests can then be run
directly, as long as they're told the stack is there:

```sh
LK_JWT_E2E_STACK_RUNNING=1 cargo test --package lk-jwt-service-e2e-tests
```
