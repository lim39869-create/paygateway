# PayGateway 开发门禁 —— 与 .github/workflows/ci.yml 保持一致
# 用法:
#   make check    # 本地全量检查(fmt/build/vet/test/race)，提交前必跑
#   make test     # 仅单测
#   make race     # 竞态检测

GO ?= go
PKG ?= ./...

.PHONY: fmt-check build vet test race check

# gofmt 门禁：未格式化即失败
fmt-check:
	@unformatted=$$(gofmt -l cmd/ internal/); \
	if [ -n "$$unformatted" ]; then \
		echo "❌ 以下文件未通过 gofmt:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "✅ gofmt 通过"

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test $(PKG) -count=1

race:
	$(GO) test -race $(PKG) -count=1

# 提交前全量检查（CI 同一套）
check: fmt-check build vet race
	@echo "✅ 全部检查通过，可以提交"
