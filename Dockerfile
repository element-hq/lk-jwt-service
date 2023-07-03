FROM golang:1.20-alpine as builder

WORKDIR /proj

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY *.go ./

RUN go build -o lk-jwt-service

FROM scratch

COPY --from=builder /proj/lk-jwt-service /lk-jwt-service

EXPOSE 8080

CMD [ "/lk-jwt-service" ]
