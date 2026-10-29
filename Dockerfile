# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git

# Copy source files
COPY . .

# Build WASM binary
RUN GOOS=js GOARCH=wasm go build -ldflags="-s -w" -o web/static/mosh.wasm ./cmd/mosh-rtc-wasm

# Ensure wasm_exec.js matches the Go runtime
RUN cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" web/static/wasm_exec.js || cp "$(go env GOROOT)/misc/wasm/wasm_exec.js" web/static/wasm_exec.js

# Build server binary
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /mosh-webrtc-server main.go

# Runtime stage
FROM alpine:latest

RUN apk add --no-cache mosh-server bash ca-certificates tzdata

WORKDIR /app
COPY --from=builder /mosh-webrtc-server /app/mosh-webrtc-server
COPY --from=builder /src/web/static /app/web/static

ENV PORT=8090
ENV LANG=en_US.UTF-8
ENV LC_ALL=en_US.UTF-8
ENV TERM=xterm-256color

EXPOSE 8090

ENTRYPOINT ["/app/mosh-webrtc-server"]
