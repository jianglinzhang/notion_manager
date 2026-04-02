# Stage 1: 构建 Go + React（官方 Go 1.25 要求）
FROM golang:1.25-alpine AS builder

# 安装 Node.js（构建 React 前端）
RUN apk add --no-cache nodejs npm

WORKDIR /app

# 复制全部源码
COPY . .

# ==================== React 前端构建 ====================
RUN cd web && \
    npm ci --omit=dev && \
    npm run build && \
    mkdir -p ../internal/web && \
    cp -r dist/* ../internal/web/ || echo "⚠️ web/ 目录已预嵌入或无需构建"

# ==================== Go 编译（二进制输出到 /app/notion-manager） ====================
RUN go mod download && \
    CGO_ENABLED=0 GOOS=linux go build -o /app/notion-manager ./cmd/notion-manager

# Stage 2: 极简运行时镜像
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata && \
    adduser -D -u 1000 appuser

WORKDIR /app

# 关键修复：二进制直接复制到 WORKDIR 里
COPY --from=builder /app/notion-manager /app/notion-manager

# 创建必要目录（config.yaml、accounts 池）
RUN mkdir -p /app/accounts /app/data

# 给二进制执行权限（保险起见）
RUN chmod +x /app/notion-manager

USER appuser

EXPOSE 8081

# 现在 CMD 可以正确找到 ./notion-manager
CMD ["./notion-manager"]
