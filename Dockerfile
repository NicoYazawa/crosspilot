# CrossPilot Go 服务多阶段构建
#
# 构建: docker build -t crosspilot-app .
# 启动前自动执行 pending migrations（幂等）

FROM golang:1.27-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN GOPROXY=https://goproxy.cn,direct go mod download

COPY . .

ARG CGO_ENABLED=0

RUN go build -ldflags="-s -w" -o /crosspilot ./cmd/server && \
    go build -ldflags="-s -w" -o /crosspilot-migrate ./cmd/migrate && \
    go build -ldflags="-s -w" -o /crosspilot-catalog-import ./cmd/catalog-import

# Final stage
FROM alpine:3.23.5

RUN apk add --no-cache ca-certificates tzdata wget

WORKDIR /app
COPY --from=build /crosspilot /app/crosspilot
COPY --from=build /crosspilot-migrate /app/crosspilot-migrate
COPY --from=build /crosspilot-catalog-import /app/crosspilot-catalog-import

# 商品引导数据随镜像发布。只放压缩件（4.6 MB → 0.31 MB），导入器按 gzip 魔数
# 自动解压，读取端不需要额外参数。
COPY data/catalog-v3.jsonl.gz /app/data/catalog-v3.jsonl.gz

EXPOSE 8000

# 启动前：执行 pending migrations → 导入商品（库非空则跳过）→ 启动 server。
#
# 商品导入用 && 串在这里而不是放进程内后台跑：库是空的而服务报健康，正是
# 之前「商品库是空的」没人发现的原因。导入失败就让容器起不来，问题在启动
# 阶段暴露，而不是等用户打开界面才发现没有商品。
#
# exec 替换 shell，使 SIGTERM 直接传给 server 进程
ENTRYPOINT ["sh", "-c", "/app/crosspilot-migrate up && /app/crosspilot-catalog-import && exec /app/crosspilot"]
