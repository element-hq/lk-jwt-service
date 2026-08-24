# End-to-end test suite

End-to-end test suite for lk-jwt-service. The suite drives the service
integrated as an application service with a real Synapse instance and
LiveKit SFU.

## Running

Requires Docker (and `docker compose` or `docker-compose`).

```sh
cd e2e-tests
cargo test
```
