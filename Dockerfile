# ========= 1) 构建前端 =========
FROM node:20-bookworm AS web-builder

WORKDIR /src/web

COPY web/package*.json ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install; fi

COPY web/ ./
RUN npm run build


# ========= 2) 构建 Go =========
FROM golang:1.25-bookworm AS go-builder

WORKDIR /src

# 先复制整个项目，避免 go.mod 有本地 replace 时 go mod download 失败
COPY . .

# 拷贝前端构建产物到 embed 目录
COPY --from=web-builder /src/web/dist /src/internal/web/dist

# 可选：设置 Go 代理，提高稳定性
RUN go env -w GOPROXY=https://proxy.golang.org,direct

# 调试信息 + 构建
RUN go version && \
    cat go.mod && \
    ls -la /src && \
    ls -la /src/internal/web && \
    go mod download && \
    go build -v -o /src/notion-manager ./cmd/notion-manager


# ========= 3) 运行镜像 =========
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=go-builder /src/notion-manager /app/notion-manager

RUN mkdir -p /app/accounts /app/data

EXPOSE 8081

CMD ["/app/notion-manager"]
