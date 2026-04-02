# Stage 1: Go 构建（官方要求 Go 1.25）
FROM golang:1.25-alpine AS builder

WORKDIR /app

# 复制全部源码（包含已预嵌入的 internal/web/）
COPY . .

# Go 依赖 + 编译（完全按官方 README 命令）
RUN go mod download && \
    CGO_ENABLED=0 GOOS=linux go build -o /app/notion-manager ./cmd/notion-manager

# Stage 2: 极简运行时
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata && \
    adduser -D -u 1000 appuser

WORKDIR /app

# 关键：复制二进制 + 权限
COPY --from=builder /app/notion-manager /app/notion-manager
RUN chmod +x /app/notion-manager && \
    mkdir -p /app/accounts /app/data

USER appuser

EXPOSE 8081

# 使用绝对路径 CMD（彻底杜绝路径问题）
CMD ["/app/notion-manager"]
