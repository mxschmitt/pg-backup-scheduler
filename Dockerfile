# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum* ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o backup ./cmd/backup
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o cli ./cmd/cli

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates docker-cli wget tzdata

WORKDIR /app

# Copy binaries from builder
COPY --from=builder /build/backup .
COPY --from=builder /build/cli /usr/local/bin/cli

# Create directories for backups and sockets
RUN mkdir -p /data/backups /tmp

EXPOSE 8080

CMD ["./backup"]
