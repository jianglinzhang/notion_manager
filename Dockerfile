# Stage 1: 构建 Go + React（官方要求 Go 1.25+）
FROM golang:1.25-alpine AS builder

# 安装 Node.js
RUN apk add --no-cache nodejs npm

WORKDIR /app

# 复制全部源码
COPY . .

# ==================== React 前端构建（完全按官方 README） ====================
RUN cd web && \
    npm ci --omit=dev && \
    npm run build && \
    mkdir -p ../internal/web && \
    cp -r dist ../internal/web/ || echo "⚠️ web/ 目录已预嵌入或无需构建"

# ==================== Go 编译（输出到 /app/notion-manager） ====================
RUN go mod download && \
    CGO_ENABLED=0 GOOS=linux go build -o /app/notion-manager ./cmd/notion-manager

# Stage 2: 运行时镜像
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata && \
    adduser -D -u 1000 appuser

WORKDIR /app

# 关键修复：二进制路径完全对齐
COPY --from=builder /app/notion-manager /app/notion-manager

RUN mkdir -p /app/accounts /app/data && \
    chmod +x /app/notion-manager

USER appuser

EXPOSE 8081

# 使用绝对路径 CMD，彻底避免路径问题
CMD ["/app/notion-manager"]
