GO := go
GENERATOR := bin/oapi-cli-gen
DEMO_ARGS := --spec testdata/pets/openapi.yaml --name pets --module example.com/pets-cli --out dist/pets-demo

.PHONY: build demo fmt-check vet test race vulncheck check-generated check

build:
	GOWORK=off $(GO) build -trimpath -o $(GENERATOR) ./cmd/oapi-cli-gen

demo: build
	@set -eu; if [ -d dist/pets-demo ]; then $(GENERATOR) generate $(DEMO_ARGS) --update; else $(GENERATOR) generate $(DEMO_ARGS); fi
	GOWORK=off $(GO) -C dist/pets-demo mod tidy
	GOWORK=off $(GO) -C dist/pets-demo build -trimpath -o ../../bin/pets ./cmd/pets
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
	GOWORK=off $(GO) -C dist/pets-demo vet ./...
	@set -eu; scanner="$$(GOWORK=off $(GO) tool -n govulncheck)"; \
	GOWORK=off "$$scanner" -C dist/pets-demo/cmd/pets -scan=module; \
	GOWORK=off "$$scanner" -mode=binary -scan=module bin/pets

check: fmt-check vet test race vulncheck check-generated
