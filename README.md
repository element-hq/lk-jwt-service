# LiveKit Management Service

This service is currently used for a single reason: generate JWT tokens with a given identity for a given room, so that users can use them to authenticate against LiveKit SFU.

## Usage

To start the service locally:

```
$ LIVEKIT_URL="ws://somewhere" LIVEKIT_KEY=devkey LIVEKIT_SECRET=secret go run *.go
```

The listening port is configurable via the `LK_JWT_PORT` environment variable and defaults to 8080.

Usage can be limited to a set of specific homeservers by setting `HS_ALLOWLIST` environment variable to a comma-separated list of server names.

## Disable TLS verification

For testing and debugging (e.g., in the absence of trusted certificates while testing in a lab) you can disable TLS verification for outgoing matrix client connection by setting the environment variable `LIVEKIT_INSECURE_SKIP_VERIFY_TLS` to `YES_I_KNOW_WHAT_I_AM_DOING`.
