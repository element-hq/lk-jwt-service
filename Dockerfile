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

FROM scratch

COPY --from=builder /proj/lk-jwt-service /lk-jwt-service
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8080

CMD [ "/lk-jwt-service" ]
