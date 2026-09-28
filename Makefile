GO ?= go

D2_SOURCES := docs/architecture.d2 $(wildcard docs/icons/*.svg)

.PHONY: all

all: docs/architecture.svg

docs/architecture.svg: $(D2_SOURCES)
	$(GO) tool d2 docs/architecture.d2 $@
