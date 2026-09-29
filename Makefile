GO ?= go
BUILD_DIR ?= build
IMAGE ?= localhost/sunbeam:dev
SUNBEAM_BINARY := $(BUILD_DIR)/sunbeam
SUNBEAM_SOURCES := $(shell find cmd/sunbeam internal/gen/sunbeam internal/modules/routing -name '*.go') go.mod go.sum Makefile

D2_SOURCES := docs/diagrams/architecture.d2 $(wildcard docs/diagrams/icons/*.svg)
PROTOC_GEN_GO := $(shell $(GO) tool -n protoc-gen-go)
PROTOC_GEN_CONNECT_GO := $(shell $(GO) tool -n protoc-gen-connect-go)
PROTO_SOURCES := $(wildcard api/sunbeam/v1/*.proto)

.PHONY: all clean generate image sunbeam

all: docs/diagrams/architecture.png

sunbeam: $(SUNBEAM_BINARY)

$(SUNBEAM_BINARY): $(SUNBEAM_SOURCES)
	mkdir -p "$(@D)"
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -ldflags='-s -w' -o "$@" ./cmd/sunbeam

image: sunbeam
	@set -eu; \
	container=""; \
	trap 'if [ -n "$$container" ]; then buildah rm "$$container" >/dev/null 2>&1 || true; fi' EXIT HUP INT TERM; \
	container="$$(buildah from scratch)"; \
	buildah copy "$$container" "$(SUNBEAM_BINARY)" /sunbeam >/dev/null; \
	buildah config --entrypoint '["/sunbeam"]' --port 80/tcp --volume /var/lib/sunbeam "$$container"; \
	buildah commit --rm "$$container" "$(IMAGE)" >/dev/null; \
	container=""; \
	echo "Built $(IMAGE)"

clean:
	rm -rf -- "$(BUILD_DIR)"

generate:
	protoc -I api -I third_party/googleapis -I /usr/include \
		--plugin=protoc-gen-go=$(PROTOC_GEN_GO) \
		--plugin=protoc-gen-connect-go=$(PROTOC_GEN_CONNECT_GO) \
		--go_out=. --go_opt=module=github.com/goevery/sunstone \
		--connect-go_out=. --connect-go_opt=module=github.com/goevery/sunstone \
		$(PROTO_SOURCES)
	$(GO) tool mockery

docs/diagrams/architecture.png: $(D2_SOURCES)
	$(GO) tool d2 docs/diagrams/architecture.d2 $@
