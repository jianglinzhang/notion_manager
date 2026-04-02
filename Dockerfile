# ========= 1. 构建前端 =========
FROM node:20-bookworm AS web-builder

WORKDIR /app/web

COPY web/package*.json ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install; fi

COPY web/ ./
RUN npm run build


# ========= 2. 构建 Go 后端 =========
FROM golang:1.24-bookworm AS go-builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 按 README 的方式，把前端 dist 拷贝到 embed 目录
COPY --from=web-builder /app/web/dist /app/internal/web/dist

# 直接 go build，不要加 CGO_ENABLED=0
RUN go build -v -o /app/notion-manager ./cmd/notion-manager


# ========= 3. 运行镜像 =========
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=go-builder /app/notion-manager /app/notion-manager

RUN mkdir -p /app/accounts /app/data

EXPOSE 8081

CMD ["/app/notion-manager"]
