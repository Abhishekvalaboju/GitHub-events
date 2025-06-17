# --- Stage 1: Build ---
FROM golang:1.24-alpine AS builder

# Set working directory inside the container
WORKDIR /app

# Copy go mod and sum first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the code
COPY . .

# Build the Go app
RUN go build -o webhook-server main.go

# --- Stage 2: Run ---
FROM alpine:latest

WORKDIR /root/

# Copy the built binary from the builder stage
COPY --from=builder /app/webhook-server .

# Expose port if needed (optional)
EXPOSE 8080

# Command to run the binary
ENTRYPOINT ["./webhook-server"]
