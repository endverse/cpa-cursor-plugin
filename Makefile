.PHONY: build

PLUGIN = cursor-agent

build:
	mkdir -p dist
	CGO_ENABLED=1 go build -buildmode=c-shared -o dist/$(PLUGIN).so .
