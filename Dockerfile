# Stage 1: builder
FROM golang:1.23-alpine AS builder

WORKDIR /build

# Cache module downloads
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source
COPY . .

# Build static binary (CGO_ENABLED=0, pure Go with modernc.org/sqlite)
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X github.com/wnarutou/gitrieve/internal/buildinfo.Version=${VERSION}" -o gitrieve main.go

# Stage 2: runtime
FROM alpine:latest

# ca-certificates for HTTPS (git servers), git for cloning repos,
# tzdata for cron schedules configured through the TZ environment variable
RUN apk add --no-cache ca-certificates git tzdata

WORKDIR /app

# Copy the static binary (web assets are embedded in the binary)
COPY --from=builder /build/gitrieve /app/gitrieve

EXPOSE 8080

ENTRYPOINT ["/app/gitrieve"]
CMD ["server"]
