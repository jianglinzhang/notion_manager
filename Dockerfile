# Stage 1: 构建 Go + React（单 builder 阶段，简单可靠）
FROM golang:1.24-alpine AS builder

# 安装 Node.js（用于构建 React 前端）
RUN apk add --no-cache nodejs npm

WORKDIR /app

# 复制全部源码
COPY . .

# 构建 React 前端（完全按照官方 README 的命令）
RUN cd web && \
    npm ci && \
    npm run build && \
    cp -r dist ../internal/web/

# 下载 Go 依赖 + 编译二进制（静态链接，适合 Docker）
RUN go mod download && \
    CGO_ENABLED=0 GOOS=linux go build -o /notion-manager ./cmd/notion-manager

# Stage 2: 极简运行时镜像
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata && \
    adduser -D -u 1000 appuser

WORKDIR /app

# 复制编译好的二进制
COPY --from=builder /notion-manager /notion-manager

# 创建必要的目录（config.yaml 和 accounts 池会自动生成）
RUN mkdir -p /app/accounts /app/data

# 切换非 root 用户（安全）
USER appuser

EXPOSE 8081

# 启动命令（可通过环境变量覆盖 config）
CMD ["./notion-manager"]
