# Build go
FROM golang:1.26.1-alpine AS builder
WORKDIR /app
COPY . .
ENV CGO_ENABLED=0
RUN GOEXPERIMENT=jsonv2 go mod download
RUN GOEXPERIMENT=jsonv2 go build -v -o znode

# Release
FROM  alpine
# Cài đặt các gói công cụ cần thiết
RUN  apk --update --no-cache add tzdata ca-certificates \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN mkdir /etc/znode/
COPY --from=builder /app/znode /usr/local/bin

ENTRYPOINT [ "znode", "server", "--config", "/etc/znode/config.json"]
