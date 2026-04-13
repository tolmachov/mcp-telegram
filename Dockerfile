FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o mcp-telegram .

FROM alpine:latest

WORKDIR /root/

COPY --from=builder /app/mcp-telegram .

ENTRYPOINT ["./mcp-telegram"]
