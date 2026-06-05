# Set the version to match that which is in go.mod
ARG GO_VERSION="build-arg-must-be-provided"

FROM --platform=${BUILDPLATFORM} docker.io/golang:${GO_VERSION}-alpine AS builder

WORKDIR /proj

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY *.go ./
COPY healthcheck ./healthcheck

ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o lk-jwt-service
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o lk-jwt-service-healthcheck ./healthcheck
# set up nsswitch.conf for Go's "netgo" implementation
# - https://github.com/golang/go/blob/go1.24.0/src/net/conf.go#L343
RUN echo 'hosts: files dns' > /etc/nsswitch.conf

FROM scratch

COPY --from=builder /proj/lk-jwt-service /lk-jwt-service
COPY --from=builder /proj/lk-jwt-service-healthcheck /lk-jwt-service-healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /etc/nsswitch.conf /etc/nsswitch.conf

EXPOSE 8080

CMD [ "/lk-jwt-service" ]
HEALTHCHECK --interval=30s --timeout=3s --retries=3 CMD ["/lk-jwt-service-healthcheck"]
