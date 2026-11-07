# Multi-stage Dockerfile for XeroxGo Cloud Backend
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Copy Go workspaces and dependency manifests
COPY go.work go.work.sum* ./
COPY cloud-backend/go.mod cloud-backend/go.sum* ./cloud-backend/
COPY kiosk-daemon/go.mod kiosk-daemon/go.sum* ./kiosk-daemon/
COPY pkg/contracts/go.mod* ./pkg/contracts/

# Copy monorepo source
COPY . .

# Build statically linked cloud backend server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/xeroxgo-cloud ./cloud-backend/cmd/server

# Minimal runtime image
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata qpdf ghostscript libreoffice ttf-dejavu font-noto

WORKDIR /app

# Prepare storage directories
RUN mkdir -p /data/jobs && chmod 700 /data/jobs

COPY --from=builder /bin/xeroxgo-cloud /app/xeroxgo-cloud

ENV PORT=8080 \
    XEROXGO_CLOUD_DB=/data/xeroxgo-cloud.db \
    XEROXGO_JOBS_DIR=/data/jobs \
    XEROXGO_PRICE_PER_SHEET_PAISE=500 \
    XEROXGO_DEV_MODE=0

EXPOSE 8080

ENTRYPOINT ["/app/xeroxgo-cloud"]
