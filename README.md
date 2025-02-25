# LiveKit Token Management Service

This service is currently used for a single reason: generate JWT tokens with a given identity for a given room, so that users can use them to authenticate against LiveKit SFU.

## Usage

Livekit Token management Service is required to host the element-call.

Element-call requires the following services to work:
1. Element-call
2. Livekit-SFU
3. Livekit Token Management System (this repo).


## Installation

### From release file

1. Download the tar file:
```   
wget or fetch https://github.com/element-hq/lk-jwt-service/archive/refs/tags/v0.1.1.tar.gz

tar -xvf v0.1.1.tar.gz

mv lk-jwt-service-0.1.1 lk-jwt-service

cd lk-jwt-service

go build -o lk-jwt-service .

```

To start the service locally:

```
$ LIVEKIT_URL="ws://somewhere" LIVEKIT_KEY=devkey LIVEKIT_SECRET=secret ./lk-jwt-service
```

The listening port is configurable via the `LK_JWT_PORT` environment variable and defaults to 8080.

## Disable TLS verification

For testing and debugging (e.g., in the absence of trusted certificates while testing in a lab) you can disable TLS verification for outgoing matrix client connection by setting the environment variable `LIVEKIT_INSECURE_SKIP_VERIFY_TLS` to `YES_I_KNOW_WHAT_I_AM_DOING`.
