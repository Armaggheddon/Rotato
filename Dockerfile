# Stage 1: Builder
FROM golang:1.27-alpine AS builder
# Target CPU architecture. BuildKit sets TARGETARCH automatically to the build
# host's arch; docker compose passes it through as a build arg (see
# docker-compose.yml), so `TARGETARCH=arm64 docker compose build` cross-builds
# an arm64 image (Raspberry Pi) even on an x86 machine — Go cross-compiles
# natively with CGO_ENABLED=0, no QEMU/binfmt needed. Empty = build host arch.
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o rotato .

# Stage 2: Scratch
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/rotato /rotato
# Document the port (docker run -P and tooling pick it up; compose maps 8080).
EXPOSE 8080
ENTRYPOINT ["/rotato"]
