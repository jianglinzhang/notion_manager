# Stage 1: Go 构建（官方要求 Go 1.25）
FROM golang:1.25-alpine AS builder

WORKDIR /app

# 复制全部源码
COPY . .

# ==================== 构建 + 详细调试日志 ====================
RUN go mod download && \
    echo "=== 当前目录文件 ===" && ls -la && \
    echo "=== 开始 go build ===" && \
    CGO_ENABLED=0 GOOS=linux go build -o /app/notion-manager ./cmd/notion-manager && \
    echo "=== 构建完成，检查二进制 ===" && \
    ls -la /app/notion-manager || echo "❌ 二进制生成失败！" && \
    echo "=== 二进制大小 ===" && \
    ls -lh /app/notion-manager

# Stage 2: 运行时镜像
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata && \
    adduser -D -u 1000 appuser

WORKDIR /app

# 复制二进制 + 权限
COPY --from=builder /app/notion-manager /app/notion-manager

RUN chmod +x /app/notion-manager && \
    mkdir -p /app/accounts /app/data

USER appuser

EXPOSE 8081

# 绝对路径启动
CMD ["/app/notion-manager"]
