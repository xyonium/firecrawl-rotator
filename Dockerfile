# Build stage
FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# Static build, stripped, for scratch
ENV CGO_ENABLED=0
RUN go build -ldflags="-s -w" -o /out/api-key-rotator .

# Final stage: scratch + CA certs + binary
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/api-key-rotator /api-key-rotator
EXPOSE 8788
ENTRYPOINT ["/api-key-rotator"]
