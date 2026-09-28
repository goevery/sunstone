GO ?= go

D2_SOURCES := docs/diagrams/architecture.d2 $(wildcard docs/diagrams/icons/*.svg)

.PHONY: all generate

all: docs/diagrams/architecture.png

generate:
	$(GO) tool mockery

docs/diagrams/architecture.png: $(D2_SOURCES)
	$(GO) tool d2 docs/diagrams/architecture.d2 $@
