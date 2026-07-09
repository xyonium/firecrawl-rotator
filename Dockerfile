# Build stage
FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# Static build, stripped, for scratch
ENV CGO_ENABLED=0
RUN go build -ldflags="-s -w" -o /out/rotator .

# Final stage: scratch + CA certs + binary
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/rotator /rotator
EXPOSE 8788
ENTRYPOINT ["/rotator"]
