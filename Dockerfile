FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o droid2api .

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/droid2api .
COPY config.example.yaml ./config.yaml
EXPOSE 3000
VOLUME ["/app/data", "/app/logs"]
CMD ["./droid2api"]
