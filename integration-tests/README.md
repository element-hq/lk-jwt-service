# Integration test suite

Black-box integration tests for lk-jwt-service. The suite builds the real
service binary, runs it as a separate process and talks to it exclusively
over its external HTTP interface. The services outgoing interfaces are
connected to fake services that are used to configure the environment
and verify test results.

```
     ┌───────────────────────────────────────────────┐
     │               #[tokio::test]                  │───────────┐
     └──────────────────────┬────────────────────────┘           │
                            │ HTTP                               │
                            ▼                                    │
              ┌────────────────────────────┐                     │
              │       lk-jwt-service       │                     │
              │       (child process)      │                     │
              └───┬──────────┬─────────┬───┘                     │
                  │          │         │                         │
               C-S API     Twirp     RESP2                       │
                  │          │         │                         │
                  ▼          ▼         ▼                         │
     ┌────────────────┐ ┌─────────┐ ┌───────────┐                │
     │ FakeHomeserver │ │ FakeSfu │ │ FakeRedis │                │
     └────────────────┘ └─────────┘ └───────────┘                │
                  ▲          ▲         ▲                         │
                  └──────────┴─────────┴──── configure / verify ─┘
```

## Running

```sh
cd integration-tests
cargo test
```

The harness builds the service from the repository root on first use.
