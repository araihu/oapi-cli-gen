GO := go
MOCO_SOURCE ?= ../moco
GENERATOR := bin/oapi-cli-gen
DEMO_ARGS := --spec testdata/moco/public.yaml --name moco --module github.com/araihu/oapi-cli-gen/examples/moco --out dist/moco

.PHONY: build demo fmt-check vet test race vulncheck check-generated check smoke-moco

build:
	GOWORK=off $(GO) build -trimpath -o $(GENERATOR) ./cmd/oapi-cli-gen

demo: build
	@set -eu; if [ -d dist/moco ]; then $(GENERATOR) generate $(DEMO_ARGS) --update; else $(GENERATOR) generate $(DEMO_ARGS); fi
	GOWORK=off $(GO) -C dist/moco mod tidy
	GOWORK=off $(GO) -C dist/moco build -trimpath -o ../../bin/moco ./cmd/moco
	$(GENERATOR) generate $(DEMO_ARGS) --check

fmt-check:
	@test -z "$$(gofmt -l cmd generator internal)" || { echo 'Go files need formatting'; exit 1; }

vet:
	GOWORK=off $(GO) vet ./...

test:
	GOWORK=off $(GO) test ./... -count=1

race:
	GOWORK=off $(GO) test -race ./... -count=1

vulncheck:
	GOWORK=off $(GO) tool govulncheck -C generator -scan=module

check-generated: demo
	GOWORK=off $(GO) -C dist/moco vet ./...
	@set -eu; scanner="$$(GOWORK=off $(GO) tool -n govulncheck)"; \
	GOWORK=off "$$scanner" -C dist/moco/cmd/moco -scan=module; \
	GOWORK=off "$$scanner" -mode=binary -scan=module bin/moco

check: fmt-check vet test race vulncheck check-generated

smoke-moco: demo
	@set -eu; test -f "$(MOCO_SOURCE)/go.mod"; \
	build_dir=$$(mktemp -d); trap 'rm -rf "$$build_dir"' EXIT; \
	GOWORK=off $(GO) -C "$(MOCO_SOURCE)" build -trimpath -o "$$build_dir/moco-server" ./cmd/moco-server; \
	python3 -B scripts/smoke_moco.py --server "$$build_dir/moco-server" --cli bin/moco
