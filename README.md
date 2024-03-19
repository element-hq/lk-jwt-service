# LiveKit Management Service

This service is currently used for a single reason: generate JWT tokens with a given identity for a given room, so that users can use them to authenticate against LiveKit SFU.

## Usage

To start the service locally:

```
$ LIVEKIT_URL="ws://somewhere" LIVEKIT_KEY=devkey LIVEKIT_SECRET=secret go run *.go
```

The listening port is configurable via the `LK_JWT_PORT` environment variable.