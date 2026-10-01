# ---- 构建阶段 ----
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 静态资源（static/openapi.yaml、swagger-ui）通过 go:embed 打包进二进制
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/passgate .

# ---- 运行阶段 ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata redis && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && echo "Asia/Shanghai" > /etc/timezone
WORKDIR /app
COPY --from=builder /out/passgate /app/passgate
EXPOSE 8080
ENTRYPOINT ["/app/passgate"]
