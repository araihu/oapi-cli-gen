# oapi-cli-gen

Generate a standalone Go command-line application from an OpenAPI 3.0 or 3.1
contract. The generator uses **oapi-codegen v2.8.1** for models, the embedded
contract, and typed HTTP request constructors; **Cobra** supplies the command
tree, help, flags and shell completion.

The generated application has its own `go.mod`. Its source contains the client
and CLI runtime; it does not import or execute this generator at runtime.

CLI naming metadata can be maintained in a separate OpenAPI Overlay and merged
by the API producer with Vacuum. The generator consumes the prepared bundle;
it does not merge overlays or infer short command names from resource names.

## Generate a CLI

Go 1.27.3 or newer is required. `mise` selects the latest stable Go.

```sh
go install github.com/araihu/oapi-cli-gen/cmd/oapi-cli-gen@latest

oapi-cli-gen generate \
  --spec ./openapi.cli.json \
  --name inventory \
  --module example.com/inventory-cli \
  --out ./inventory-cli

cd inventory-cli
go mod tidy
go build -trimpath -o inventory ./cmd/inventory
./inventory --help
./inventory completion bash
```

`--out` must be a new directory. Generation validates the contract and produces:

```text
cmd/inventory/main.go             executable entrypoint and signal handling
internal/api/client.gen.go        oapi-codegen models/client/embedded contract
internal/cli/commands.gen.go      typed request-constructor adapters
internal/cliruntime/*.gen.go      Cobra, input, HTTP, output and authentication
go.mod                           pinned dependency floors
.oapi-cli-gen.json                contract fingerprint and managed-file hashes
```

The `generator` Go package exposes the same pipeline for build tools:

```go
result, err := generator.Generate(ctx, generator.Options{
    Spec: "openapi.cli.json", Name: "inventory", Module: "example.com/inventory-cli",
})
if err != nil { return err }
return result.Write("inventory-cli", false)
```

## Contract-to-command mapping

- The first operation tag becomes a command group: `Secrets` becomes `secrets`.
- `operationId` becomes the leaf command: `getSecret` becomes `get-secret`.
- Untagged operations live directly under the root command.
- Missing operation IDs are derived deterministically from method and path.
- Path, query, header and cookie parameters become flags: `tenantId` becomes
  `--tenant-id`; `If-Match` becomes `--if-match`.
- A colliding parameter flag gains its location prefix: a query named `server`
  becomes `--query-server`, preserving the global `--server` option.
- Arrays use repeated flags (`--tag first --tag second`). Objects use JSON.
  Booleans accept both `--enabled` and `--enabled=false`.
- Inherited path-item parameters and operation-level overrides are respected.
- Types, defaults, required fields, enums and schema constraints are validated
  before HTTP. JSON bodies are validated as requests against the embedded schema.
  The generated SDK performs the actual OpenAPI parameter serialization.

CLI extensions customize the interface without hand-editing Go. Keep them in
a separate overlay so the principal API contract stays neutral:

```yaml
overlay: 1.0.0
info:
  title: Inventory CLI metadata
  version: 1.0.0
actions:
  - target: $.paths.*[?(@.operationId == 'getSecret')]
    update:
      x-cli-name: get
      x-cli-group: secrets
```

The producer applies the overlay to a self-contained YAML bundle, then uses
Vacuum to produce the CLI input bundle:

```sh
vacuum bundle --composed openapi.yaml openapi.bundle.yaml
vacuum apply-overlay --fail-on-warnings openapi.bundle.yaml cli.overlay.yaml openapi.cli.yaml
vacuum bundle --composed --format json openapi.cli.yaml openapi.cli.json
```

This exposes `inventory secrets get`. The principal operation ID remains
`getSecret`; without metadata, the default command remains `get-secret`.
`x-cli-flag` can rename a parameter flag, and `x-cli-ignore: true` can exclude
an operation, through overlay actions targeting those elements. Names and
collisions are checked; API-domain behavior belongs to the producer's metadata.

## Authentication, TLS and input

For a generated executable named `inventory`, the base URL can be supplied by
`INVENTORY_SERVER` or `--server`. Otherwise the first root-level OpenAPI server
is used, with its declared variable defaults. Relative server URLs require an
explicit deployment URL. Base-path prefixes are preserved.

Bearer, OAuth2 and OpenID Connect schemes consume an existing token from
`INVENTORY_TOKEN`. `--auth-env securityScheme=ENV_NAME` overrides the environment
variable for any security scheme. API keys default to
`INVENTORY_<SECURITY_SCHEME_NAME>` and support header, query and cookie placement.
HTTP Basic uses `username:password` in its selected environment variable.
Security alternatives (OR), combined schemes (AND), and explicitly anonymous
operations follow the contract. Anonymous operations do not receive credentials.

```sh
export INVENTORY_SERVER=https://api.example.com
# Supply INVENTORY_TOKEN through your existing credential mechanism.
inventory pets get-pet --pet-id 42
inventory pets create-pet --body-file ./request.json
inventory pets create-pet --body-stdin < ./request.json
inventory pets get-pet --pet-id 42 --response-header ETag
```

Credentials have no inline token flag and are never persisted by the CLI.
Request bodies come from `--body-file`, `--body-file -`, or `--body-stdin`.
`--content-type` selects a declared media type; JSON is preferred when available.
Raw bodies support other media types, including pre-encoded forms/multipart.

TLS certificate verification is enabled. `--ca-file` adds a PEM trust bundle;
`--client-cert` and `--client-key` support mutual TLS. HTTP is allowed on
loopback; other HTTP deployments require `--allow-http`. Server URLs cannot
contain credentials, query strings or fragments. Redirects are never followed.

There are no automatic retries: conditional headers and idempotency parameters
remain explicit contract flags. `--timeout` bounds the entire request and defaults
to one minute. Request bodies default to an 8 MiB limit and responses to 64 MiB;
`--max-input-bytes` and `--max-output-bytes` configure those bounds.

## Output and automation

Successful responses use an aligned table on stdout by default. Select
`--output table`, `--output json` or `--output csv` explicitly:

```sh
moco tenants list                 # default table
moco tenants list --output json   # complete response, including pagination
moco tenants list --output csv    # header and one CSV record per item
```

Tables and CSV use sorted field names as columns. Objects produce one row;
arrays produce one row per element. For an object containing an `items` array,
they show those items; use JSON to retain the full envelope and pagination.
Missing fields are empty, JSON null is `null`, and nested values use compact
JSON cells. Numbers retain their original precision. Table cells escape control
characters; CSV preserves values with standard quoting, including embedded
commas, quotes and newlines. Empty collections produce no table or CSV output.

`--output json` pretty-prints the complete JSON response. `--output raw` streams
raw bytes; `--response-header ETag` prints one header instead of any body format.
Empty responses and conditional HTTP 304 responses succeed without body output.
Response data can contain secrets: stdout is intended for the caller's chosen
destination. Credentials, request values and untrusted HTTP error bodies are
not included in error diagnostics.

| Exit code | Meaning |
| --- | --- |
| 0 | Successful HTTP result, including 304; or help/completion |
| 2 | Invalid flags/input/schema, missing credentials, or configuration |
| 3 | Transport/TLS/timeout/cancellation or response read/format/size failure |
| 4 | Unsuccessful HTTP status, including an unfollowed redirect |

Raw output may be partial if streaming fails or the response exceeds its bound;
the exit status reports that failure. Table, JSON and CSV output are buffered:
invalid JSON or a response/formatting byte-limit failure produces no partial
output. Scripts that parse responses must select `--output json` or `--output csv`.

## Regeneration and reproducibility

```sh
oapi-cli-gen generate --spec openapi.cli.json --name inventory \
  --module example.com/inventory-cli --out inventory-cli --check

oapi-cli-gen generate --spec openapi.cli.json --name inventory \
  --module example.com/inventory-cli --out inventory-cli --update
```

`--check` is read-only and rejects generated drift. `--update` requires the
manifest, refuses edited generated files, and preserves user files and
`go.mod`/`go.sum`. Files are replaced atomically; a directory-wide update is not
a single filesystem transaction. Update dependency floors first if regeneration
requires a newer module version. `go mod tidy` changes are supported.

Only local contracts and references within the contract's directory are loaded.
Remote references and references escaping that directory, including symlinks,
are refused. Vendor external contracts locally for reproducible generation.
Inputs are bounded to 32 MiB across documents. Generation does not run a shell,
execute Go, or download referenced URLs. Build generated code from reviewed
contracts; explicit custom Go types/imports are code-generation inputs.
`--require module@version` pins modules used by custom `x-go-type-import` hints.

## Moco acceptance example

Moco owns the principal contracts, separate `openapi/overlays/cli.*.yaml`
metadata, and Vacuum-produced `openapi/bundled/cli/*.json` distributions.
The checked-in fixtures mirror the Moco revision pinned in the CI workflow.
The demo consumes `testdata/moco/cli/public.json`; the generator never decorates
the principal Moco contract itself.

```sh
make demo
bin/moco --help
bin/moco secrets get --help

export MOCO_SERVER=https://moco.example.com:8443
# Supply MOCO_TOKEN through your credential mechanism.
bin/moco tenants list
bin/moco secrets get --tenant-id TENANT --vault-id VAULT --path prod/database

make smoke-moco MOCO_SOURCE=../moco
```

The smoke test starts its own temporary real Moco server and exercises tenant,
vault and secret lifecycles, conditional requests, missing credentials and
transactional audit. It accepts no deployed URL or production credentials.
Both Moco CLI bundles also generate and compile in the automated tests, which
check the explicit short names and contract equivalence. CI compares the
principal bundles, overlays and CLI bundles with the pinned Moco checkout.
Use `make refresh-moco-fixtures` after rebuilding the producer; details live in
[`testdata/moco/README.md`](testdata/moco/README.md).

## Development and current scope

```sh
make check
```

Checks include formatting, vet, unit and executable integration tests, the race
detector, deterministic generation, reference confinement, safe regeneration,
and module-level vulnerability scans of both generator and generated client/binary. CI also
executes the real Moco smoke test at the pinned server revision.

The current scope is OpenAPI 3.0/3.1 request/response operations with schema-based
parameters. Swagger 2, OpenAPI 3.2, parameter `content`, callback/webhook listeners,
interactive OAuth login, stored profiles, automatic pagination/retries, and
field-by-field body editors are not implemented. The CLI sends to one configured
base URL per invocation. Non-JSON bodies are sent as provided; schema body
validation currently covers JSON media types. There is no UI or TUI.
