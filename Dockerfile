FROM docker.io/rust:1-alpine AS builder

WORKDIR /proj

# musl-dev for the static libc, cmake/make/gcc/g++/perl/linux-headers for the
# vendored aws-lc-sys / ring C code pulled in by rustls.
RUN apk add --no-cache musl-dev cmake make gcc g++ perl linux-headers ca-certificates

COPY Cargo.toml Cargo.lock ./
COPY src ./src

RUN cargo build --release --locked
RUN cp target/release/lk-jwt-service /lk-jwt-service \
    && cp target/release/healthcheck /lk-jwt-service-healthcheck

FROM scratch

COPY --from=builder /lk-jwt-service /lk-jwt-service
COPY --from=builder /lk-jwt-service-healthcheck /lk-jwt-service-healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8080

CMD [ "/lk-jwt-service" ]
HEALTHCHECK --interval=30s --timeout=3s --retries=3 CMD ["/lk-jwt-service-healthcheck"]
