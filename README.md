# LiveKit Token Management Service

This service is used for two reasons:
- generate JWT tokens with a given LiveKit identity for a given LiveKit room, so that users can use them to authenticate against LiveKit SFU,
- In case of local Matrix users which belong to the same deployment, the corresponding LiveKit room on the SFU will be created if necessary.

It works by allowing a token obtained via the Matrix Client-Server API [OpenID endpoint](https://spec.matrix.org/v1.13/client-server-api/#openid) to be exchanged for a LiveKit JWT token which can be used to access a LiveKit SFU.

This functionality is defined by [MSC4195: MatrixRTC using LiveKit backend](https://github.com/matrix-org/matrix-spec-proposals/pull/4195).

Only for Matrix users of homeservers belonging to the same deployment (called local users) corresponding rooms on the LiveKit SFU will be automatically created. Hence, local homeservers need to be declared via the `LIVEKIT_FULL_ACCESS_HOMESERVERS` environment variable (see below).

Note access to LiveKit SFU is restricted for remote users (not belonging to the same deployment). Those users can join existing LiveKit SFU rooms, but missing rooms will not be automatically created to prevent misuse of infrastructure. Due to the SFU selection algorithm and the order of events this will NOT limit or prevent video conferences across Matrix federation.

## Usage

This service is used when hosting the [Element Call](https://github.com/element-hq/element-call) video conferencing application against a LiveKit backend.

Alongside this service, you will need the [LiveKit SFU](https://github.com/livekit/livekit) and for single page applications (SPA) the [Element Call](https://github.com/element-hq/element-call) web application.

## Installation

The available releases can be found [here](https://github.com/element-hq/lk-jwt-service/releases).

### From docker image

```shell
docker run -e LIVEKIT_URL="ws://somewhere" -e LIVEKIT_KEY=devkey -e LIVEKIT_SECRET=secret -p 8080:8080 ghcr.io/element-hq/lk-jwt-service:0.1.2
```

### From release file

1. Download the tar file from the URL on the release page:

```shell
wget https://github.com/element-hq/lk-jwt-service/archive/refs/tags/v0.1.1.tar.gz
tar -xvf v0.1.1.tar.gz
mv lk-jwt-service-0.1.1 lk-jwt-service
```

2. Build the service:

```shell
cd lk-jwt-service
go build -o lk-jwt-service .
```

3. To start the service locally:

```shell
LIVEKIT_URL="ws://somewhere" LIVEKIT_KEY=devkey LIVEKIT_SECRET=secret ./lk-jwt-service
```

### Configuration

The service is configured via environment variables:

Variable | Description | Required
--- | --- | ---
`LIVEKIT_URL` | The websocket URL of the LiveKit SFU | Yes
`LIVEKIT_KEY` or `LIVEKIT_KEY_FROM_FILE` | The API key or key file path for the LiveKit SFU | Yes
`LIVEKIT_SECRET` or `LIVEKIT_SECRET_FROM_FILE` | The secret or secret file path for the LiveKit SFU | Yes
`LIVEKIT_KEY_FILE` | file path to LiveKit SFU key-file format (`APIkey: secret`) | mutually exclusive with `LIVEKIT_KEY` and `LIVEKIT_SECRET`
`LIVEKIT_JWT_PORT` | The port the service listens on | No - defaults to 8080
`LIVEKIT_FULL_ACCESS_HOMESERVERS` | Comma-separated list of Matrix homeservers whose users are authorized with full access to LiveKit SFU features | Yes

Please double check that LiveKit SFU room default settings ([config.yaml](https://github.com/livekit/livekit/blob/7350e9933107ecdea4ada8f8bcb0d6ca78b3f8f7/config-sample.yaml#L170)) are configured as
```
room:
  auto_create: false
```

### Reverse Proxy and well-known requirements

A sample Caddy reverse proxy and well-known configuration (the MAS authenticaion is not required for lk-jwt-service but included for information.):

```
livekit-jwt.domain.tld {
        bind xx.xx.xx.xx
        reverse_proxy  localhost:8080
}
```
```
    handle /.well-known/matrix/* {
        header Content-Type application/json
        header Access-Control-Allow-Origin *  # Only needed if accessed via browser JS

        respond /client `{
            "m.homeserver": {"base_url": "https://matrix-domain.tld"},
            "org.matrix.msc4143.rtc_foci": [{
                "type": "livekit",
                "livekit_service_url": "https://livekit-jwt.domain.tld"
            }],
            "org.matrix.msc2965.authentication": {
                "issuer": "https://auth.domain.tld/",
                "account": "https://auth.domain.tld/account"
            }
        }`
```
The service is configured via environment variables:


## Disable TLS verification

For testing and debugging (e.g. in the absence of trusted certificates while testing in a lab) you can disable TLS verification for the outgoing connection to the Matrix homeserver by setting the environment variable `LIVEKIT_INSECURE_SKIP_VERIFY_TLS` to `YES_I_KNOW_WHAT_I_AM_DOING`.
