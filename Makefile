# meerkat 构建脚本
VERSION ?= dev
BIN_DIR = dist
LDFLAGS = -s -w -X main.version=$(VERSION)

.PHONY: build build-web release-local clean

build: build-web
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/meerkat .

# 将安装脚本嵌入面板分发目录（服务端会通过 /install.sh 提供）
build-web:
	mkdir -p internal/server/web_dist
	cp -f install.sh internal/server/web_dist/install.sh

# 本地一次性输出各平台二进制（CGO_ENABLED=0 保证可移植）
release-local: build-web
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/meerkat_linux_amd64 .
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/meerkat_linux_arm64 .
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/meerkat_darwin_amd64 .
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/meerkat_darwin_arm64 .
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/meerkat_windows_amd64.exe .
	@echo "输出目录: $(BIN_DIR)/"

clean:
	rm -rf $(BIN_DIR)
