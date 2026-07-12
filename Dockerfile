# syntax=docker/dockerfile:1

# ---- build stage: compile a static, CGO-free binary ----
# --platform=$BUILDPLATFORM: in multi-arch builds the compiler always runs on
# the native build host and cross-compiles via TARGETOS/TARGETARCH (no QEMU).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# Cache module downloads separately from the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go (modernc sqlite) => CGO disabled => fully static binary.
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /bot ./cmd/bot

# ---- runtime stage: minimal image with TLS roots + timezones ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 10001 app \
 && mkdir -p /app/data \
 && chown -R app:app /app

WORKDIR /app
COPY --from=build /bot /usr/local/bin/bot
USER app

EXPOSE 8080

# Both the config and the SQLite DB live in the single mounted data/ volume.
# The DB path comes from config.yaml (storage.path: data/finance_bot.db),
# resolved relative to this workdir => /app/data/finance_bot.db.
ENTRYPOINT ["bot"]
CMD ["-config", "/app/data/config.yaml"]
