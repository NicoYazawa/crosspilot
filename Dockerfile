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
    go build -ldflags="-s -w" -o /crosspilot-migrate ./cmd/migrate

# Final stage
FROM alpine:3.23.5

RUN apk add --no-cache ca-certificates tzdata wget

WORKDIR /app
COPY --from=build /crosspilot /app/crosspilot
COPY --from=build /crosspilot-migrate /app/crosspilot-migrate

EXPOSE 8000

# 启动前执行 pending migrations，然后启动 server
# exec 替换 shell，使 SIGTERM 直接传给 server 进程
ENTRYPOINT ["sh", "-c", "/app/crosspilot-migrate up && exec /app/crosspilot"]
