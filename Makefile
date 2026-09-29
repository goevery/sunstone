GO ?= go

D2_SOURCES := docs/diagrams/architecture.d2 $(wildcard docs/diagrams/icons/*.svg)
PROTOC_GEN_GO := $(shell $(GO) tool -n protoc-gen-go)
PROTOC_GEN_CONNECT_GO := $(shell $(GO) tool -n protoc-gen-connect-go)
PROTO_SOURCES := $(wildcard api/sunbeam/v1/*.proto)

.PHONY: all generate

all: docs/diagrams/architecture.png

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
