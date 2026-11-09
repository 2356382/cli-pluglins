# CLIProxyAPI CLI Runner plugin build
# 插件编译为 c-shared 动态库，由 CLIProxyAPI 宿主进程加载。

PLUGIN_NAME := cli-runner
# macOS 下 c-shared 产物为 .dylib，Linux 下为 .so，Windows 下为 .dll。
ifeq ($(OS),Windows_NT)
  EXT := dll
else
  EXT := $(shell uname -s | tr '[:upper:]' '[:lower:]' | sed -e 's/^linux$/so/' -e 's/^darwin$/dylib/' -e 's/^.*$/so/')
endif

# 输出目录，与 CLIProxyAPI 的插件发现路径对齐。
# 宿主默认扫描 ~/.cli-proxy-api/plugins/，也可通过 config 的 plugins.dir 指定。
OUT_DIR := dist

.PHONY: all build clean install help

all: build

## build: 编译插件动态库到 $(OUT_DIR)/
build:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=1 go build -buildmode=c-shared -o $(OUT_DIR)/$(PLUGIN_NAME).$(EXT) .
	@echo "Built $(OUT_DIR)/$(PLUGIN_NAME).$(EXT)"

## install: 编译并把插件动态库拷贝到 ~/.cli-proxy-api/plugins/
install: build
	@mkdir -p ~/.cli-proxy-api/plugins
	@cp -f $(OUT_DIR)/$(PLUGIN_NAME).$(EXT) ~/.cli-proxy-api/plugins/
	@echo "Installed to ~/.cli-proxy-api/plugins/$(PLUGIN_NAME).$(EXT)"

## clean: 清理构建产物
clean:
	rm -rf $(OUT_DIR)
	@find . -name '*.h' -path './dist/*' -delete 2>/dev/null

## help: 显示帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
