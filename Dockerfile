# --- Stage 1: 构建前端 (React) ---
FROM node:22-alpine AS web-builder
WORKDIR /app/web
# 先复制 package.json 提高缓存效率
COPY web/package*.json ./
RUN npm install
# 复制 web 目录下所有源码
COPY web/ .
RUN npm run build

# --- Stage 2: 构建后端 (Go) ---
# 使用 Debian 版本的 Go 镜像，比 Alpine 更稳定，减少编译时的网络/环境兼容问题
FROM golang:1.24 AS builder

# 安装编译所需的工具
RUN apt-get update && apt-get install -y git ca-certificates && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 【关键点 1】直接复制全量代码，防止 go.mod 里的 replace 指令找不到本地文件
COPY . .

# 从前端构建阶段把 dist 复制到官方要求的 internal 路径
# 如果路径不存在则创建，并确保它是空的
RUN mkdir -p internal/web/dist && \
    cp -r web/dist/* internal/web/dist/

# 【关键点 2】设置 Go 代理并拉取依赖
# 设置 GOPROXY 确保在任何环境（包括 GitHub Runner）都能稳拉依赖
ENV GOPROXY=https://proxy.golang.org,direct

# 运行 go mod download 并捕获可能的错误日志
RUN go mod download || (echo "go mod download failed, trying go mod tidy..." && go mod tidy)

# 【关键点 3】根据官方教程编译
# -ldflags "-s -w" 减小体积
# CGO_ENABLED=0 确保生成的二进制文件可以在极简的 Alpine 镜像中运行
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /notion-manager ./cmd/notion-manager

# --- Stage 3: 极简运行环境 ---
FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

# 复制二进制文件
COPY --from=builder /notion-manager /app/notion-manager

# 预创建账号和配置存储目录
RUN mkdir -p /app/accounts /app/data

# 暴露 Notion Manager 的默认端口
EXPOSE 8081

# 启动命令
ENTRYPOINT ["/app/notion-manager"]
