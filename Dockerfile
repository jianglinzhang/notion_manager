# --- Stage 1: 构建前端 (React) ---
FROM node:22-alpine AS web-builder
WORKDIR /app/web
# 复制前端源码
COPY web/package*.json ./
RUN npm install
COPY web/ .
RUN npm run build

# --- Stage 2: 构建后端 (Go) ---
FROM golang:1.24-alpine AS builder

# 安装 git（Go 编译某些包可能需要）
RUN apk add --no-cache git

WORKDIR /app

# 1. 先下载 Go 依赖（利用 Docker 缓存）
COPY go.mod go.sum ./
RUN go mod download

# 2. 复制全部源码
COPY . .

# 3. 从上一个阶段把前端构建好的 dist 复制进来
# 注意：根据官方说明，需要放到 internal/web/ 目录下
RUN mkdir -p internal/web && \
    rm -rf internal/web/dist && \
    cp -r web/dist internal/web/

# 4. 编译 Go 二进制文件
# 禁用 CGO 以实现完全静态链接，确保在 alpine 中运行
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /notion-manager ./cmd/notion-manager

# --- Stage 3: 最终运行镜像 ---
FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# 从构建阶段复制二进制文件
COPY --from=builder /notion-manager /app/notion-manager

# 预创建数据目录
RUN mkdir -p /app/accounts /app/data

# 暴露端口 (config 默认是 8081)
EXPOSE 8081

# 启动
ENTRYPOINT ["/app/notion-manager"]
