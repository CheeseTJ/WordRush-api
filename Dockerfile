# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 纯 Go 依赖（gin / mysql driver / jwt / yaml），无 CGO
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/wordrush ./cmd/wordrush

# ---- runner ----
FROM alpine:3
# ca-certificates：MySQL TLS（preferred）与调用 Ucenter / CF AI 需要系统根证书
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S app && adduser -S app -G app

WORKDIR /app
COPY --from=build /out/wordrush /app/wordrush

# 命名卷首次挂载会继承该目录属主，保证非 root 用户可写 TTS 缓存
RUN mkdir -p /app/data && chown app:app /app/data
USER app

EXPOSE 8092
# 配置文件默认读工作目录 ./config.yaml（挂载注入），密钥走 WORDRUSH_* 环境变量覆盖
ENTRYPOINT ["/app/wordrush"]
