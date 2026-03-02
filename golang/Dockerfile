FROM golang:1.22-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o tyk-sre-tool .

# ──────────────────────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /build/tyk-sre-tool /tyk-sre-tool

EXPOSE 8080

ENTRYPOINT ["/tyk-sre-tool"]

CMD ["--address", "0.0.0.0:8080"]
