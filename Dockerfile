FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o server main.go

FROM alpine:3.21

WORKDIR /app
COPY --from=builder /app/server ./server

EXPOSE 8080
CMD ["./server"]