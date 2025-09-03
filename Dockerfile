# Use the official Golang image as build stage
FROM golang:1.24.4-alpine AS builder

# Install git for go mod download
RUN apk add --no-cache git

# Set the working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

# Use Ubuntu as final stage since it has Stockfish in repositories
FROM ubuntu:22.04

# Install Stockfish, wget for health checks, and clean up
RUN apt-get update && \
    apt-get install -y stockfish wget ca-certificates && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Create a non-root user
RUN groupadd -r chessapp && useradd -r -g chessapp chessapp

# Set working directory
WORKDIR /home/chessapp

# Copy the binary from builder stage
COPY --from=builder /app/main .

# Change ownership
RUN chown -R chessapp:chessapp /home/chessapp

# Switch to non-root user
USER chessapp

# Expose port 8080
EXPOSE 8080

# Command to run
CMD ["./main"]
