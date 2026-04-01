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

# 配置/数据/日志目录，方便 Docker 卷挂载
RUN mkdir -p /app/config /app/data /app/logs

# 配置文件路径通过环境变量指定，首次运行自动生成
ENV CONFIG_PATH=/app/config/config.yaml

EXPOSE 3000
VOLUME ["/app/config", "/app/data", "/app/logs"]
CMD ["./droid2api"]
