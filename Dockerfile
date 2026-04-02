# Stage 1: 构建 Go + React（使用官方要求的 Go 1.25）
FROM golang:1.25-alpine AS builder

# 安装 Node.js（构建 React 前端）
RUN apk add --no-cache nodejs npm

WORKDIR /app

# 复制全部源码
COPY . .

# ==================== React 前端构建（推荐保留，兼容你可能改过 web/） ====================
RUN cd web && \
    npm ci --omit=dev && \
    npm run build && \
    mkdir -p ../internal/web && \
    cp -r dist/* ../internal/web/ || echo "⚠️ web/ 目录无需构建（已预嵌入）"

# ==================== Go 依赖 + 编译 ====================
RUN go mod download && \
    CGO_ENABLED=0 GOOS=linux go build -o /notion-manager ./cmd/notion-manager

# Stage 2: 极简运行时镜像
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata && \
    adduser -D -u 1000 appuser

WORKDIR /app

COPY --from=builder /notion-manager /notion-manager

# 创建必要目录（config.yaml、accounts 池）
RUN mkdir -p /app/accounts /app/data

USER appuser

EXPOSE 8081

CMD ["./notion-manager"]
