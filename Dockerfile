# 跨平台构建镜像。
#
# 注意：DPAPI 只有 Windows 有，容器里无法自动读取 CodeArts Agent 的 secret storage。
# 在容器里请通过控制台「手工录入」或「从 session JSON 导入」提供凭据
# （session JSON 含 refresh_token 与 DPoP 私钥，因此仍可自动续期）。
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/codearts2api .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H -u 10001 codearts
COPY --from=build /out/codearts2api /usr/local/bin/codearts2api
USER 10001:10001
WORKDIR /data
VOLUME ["/data"]
EXPOSE 10087
ENTRYPOINT ["codearts2api"]
CMD ["-host", "0.0.0.0", "-port", "10087", "-data", "/data/codearts2api-data.json"]
