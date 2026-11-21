FROM golang:1.24 AS builder

WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/pool-status-board .

FROM alpine:latest

WORKDIR /app
COPY --from=builder /out/pool-status-board /app/pool-status-board

EXPOSE 8080
ENTRYPOINT ["/app/pool-status-board"]
