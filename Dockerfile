FROM docker.io/rust:1-alpine AS builder

WORKDIR /proj

# musl-dev for the static libc, cmake/make/gcc/g++/perl/linux-headers for the
# vendored aws-lc-sys / ring C code pulled in by rustls.
RUN apk add --no-cache musl-dev cmake make gcc g++ perl linux-headers ca-certificates

COPY Cargo.toml Cargo.lock ./

# Build with stub sources first so dependency compilation lands in its own
# layer and is skipped by the Docker layer cache when only src/ changes.
RUN mkdir -p src/bin \
    && echo "fn main() {}" > src/main.rs \
    && echo "fn main() {}" > src/bin/healthcheck.rs \
    && touch src/lib.rs \
    && cargo build --release --locked \
    && rm -rf src

COPY src ./src

# Docker preserves the source files' original mtimes, which predate the
# stub build above, so cargo would otherwise consider them unchanged and
# skip recompilation entirely.
RUN find src -name '*.rs' -exec touch {} + \
    && cargo build --release --locked
RUN cp target/release/lk-jwt-service /lk-jwt-service \
    && cp target/release/healthcheck /lk-jwt-service-healthcheck

FROM scratch

COPY --from=builder /lk-jwt-service /lk-jwt-service
COPY --from=builder /lk-jwt-service-healthcheck /lk-jwt-service-healthcheck
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8080

CMD [ "/lk-jwt-service" ]
HEALTHCHECK --interval=30s --timeout=3s --retries=3 CMD ["/lk-jwt-service-healthcheck"]
