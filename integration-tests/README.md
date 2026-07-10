# Integration test suite

Black-box integration tests for lk-jwt-service. The suite builds the real
service binary, runs it as a separate process and talks to it exclusively
over its external interfaces:

* HTTP endpoints (`/delegate_delayed_leave`, ...)
* the Matrix federation API the service calls to verify OpenID tokens
  (served by a fake homeserver under test control)
* the Matrix client-server API the service calls for delayed
  event actions (served by the same fake homeserver)

## Running

```sh
cd integration-tests
cargo test
```

The harness builds the service from the repository root on first use.
