# ---- 构建 ----
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(cat VERSION 2>/dev/null || echo dev)" -o /out/meerkat .

# ---- 运行 ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 1000 meerkat
COPY --from=builder /out/meerkat /usr/local/bin/meerkat
USER meerkat
WORKDIR /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["meerkat", "server", "--listen", ":8080", "--db", "/data/meerkat.db"]
