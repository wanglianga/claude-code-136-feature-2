# passgate 一键构建镜像：多阶段构建，运行镜像仅含二进制与 ca-certificates
FROM golang:1.19-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/passgate .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 appuser
WORKDIR /app
COPY --from=builder /out/passgate /app/passgate
USER appuser
EXPOSE 8080
ENV PORT=8080 \
    REDIS_ADDR=redis:6379
HEALTHCHECK --interval=10s --timeout=3s --retries=10 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/passgate"]
