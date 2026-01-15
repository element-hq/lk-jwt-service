# Set the version to match that which is in go.mod
ARG GO_VERSION="build-arg-must-be-provided"

FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION}-alpine AS builder

WORKDIR /proj

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY *.go ./

ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o lk-jwt-service
# set up nsswitch.conf for Go's "netgo" implementation
# - https://github.com/golang/go/blob/go1.24.0/src/net/conf.go#L343
RUN echo 'hosts: files dns' > /etc/nsswitch.conf

FROM scratch

COPY --from=builder /proj/lk-jwt-service /lk-jwt-service
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /etc/nsswitch.conf /etc/nsswitch.conf
# needed for health check below:
COPY --from=builder /lib/ld-musl-x86_64.so.1 /lib/ld-musl-x86_64.so.1
COPY --from=builder /bin/busybox /wget

EXPOSE 8080

CMD [ "/lk-jwt-service" ]

HEALTHCHECK --start-period=15s --start-interval=5s CMD ["/wget", "--spider", "http://localhost:8080/healthz"]
