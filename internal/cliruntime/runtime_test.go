package cliruntime

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

const runtimeSpec = `{
 "openapi":"3.0.3","info":{"title":"Runtime tests","version":"1"},
 "security":[{"bearerAuth":[]}],
 "paths":{
  "/items/{id}":{"parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"integer","format":"int64","minimum":1}}],
   "get":{"operationId":"read","tags":["Items"],"parameters":[{"name":"kind","in":"query","schema":{"type":"string","enum":["cat","dog"]}}],"responses":{"200":{"description":"ok"}}}},
  "/items":{"post":{"operationId":"create","tags":["Items"],"requestBody":{"required":true,"content":{"application/json":{"schema":{"type":"object","required":["name"],"additionalProperties":false,"properties":{"name":{"type":"string","minLength":1}}}}}},"responses":{"201":{"description":"created"}}}},
  "/public":{"get":{"operationId":"public","security":[],"responses":{"200":{"description":"ok"}}}}
 },"components":{"securitySchemes":{"bearerAuth":{"type":"http","scheme":"bearer"}}}
}`

func document(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(runtimeSpec))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func runCommand(t *testing.T, spec *openapi3.T, server, body string, args ...string) (string, string, error) {
	t.Helper()
	build := func(path string) Builder {
		return func(server string, input Input) (*http.Request, error) {
			method := "GET"
			if input.Body != nil {
				method = "POST"
			}
			return http.NewRequest(method, strings.TrimRight(server, "/")+path, bytes.NewReader(input.Body))
		}
	}
	root, err := New("demo", spec, map[string]Builder{"read": build("/items/1"), "create": build("/items"), "public": build("/public")})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(body))
	if server != "" {
		args = append([]string{"--server", server}, args...)
	}
	root.SetArgs(args)
	err = root.ExecuteContext(t.Context())
	return stdout.String(), stderr.String(), err
}

func TestHelpAndValidationNeverSendRequests(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "test-bearer")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); fmt.Fprint(w, `{}`) }))
	defer server.Close()
	for _, tc := range []struct {
		name, body string
		args       []string
		success    bool
	}{
		{"help", "", []string{"--help"}, true},
		{"group help", "", []string{"items"}, true},
		{"unknown subcommand", "", []string{"items", "typo"}, false},
		{"output format", "", []string{"public", "--output", "invalid"}, false},
		{"missing path", "", []string{"items", "read"}, false},
		{"invalid integer", "", []string{"items", "read", "--id", "private-marker"}, false},
		{"integer constraint", "", []string{"items", "read", "--id", "0"}, false},
		{"enum", "", []string{"items", "read", "--id", "1", "--kind", "private-marker"}, false},
		{"missing body", "", []string{"items", "create"}, false},
		{"body type", `{"name":42}`, []string{"items", "create", "--body-stdin"}, false},
		{"body required", `{}`, []string{"items", "create", "--body-stdin"}, false},
		{"body unknown", `{"name":"ok","unexpected":"private-marker"}`, []string{"items", "create", "--body-stdin"}, false},
		{"body JSON", `private-marker`, []string{"items", "create", "--body-stdin"}, false},
		{"body trailing", `{"name":"ok"} {}`, []string{"items", "create", "--body-stdin"}, false},
		{"body limit", `{"name":"private-marker"}`, []string{"items", "create", "--body-stdin", "--max-input-bytes", "3"}, false},
		{"body on GET", `{}`, []string{"items", "read", "--id", "1", "--body-stdin"}, false},
		{"content type", `{}`, []string{"items", "create", "--body-stdin", "--content-type", "text/plain"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := runCommand(t, document(t), server.URL, tc.body, tc.args...)
			if (err == nil) != tc.success {
				t.Fatalf("unexpected result: %v", err)
			}
			if err != nil && (ExitCode(err) != 2 || strings.Contains(err.Error(), "private-marker")) {
				t.Fatalf("unsafe validation error: %v", err)
			}
			if strings.Contains(stderr, "private-marker") {
				t.Fatal("input leaked to stderr")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("validation/help sent a request")
	}
}

func TestNestedGroupCommands(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "test-bearer")
	spec := document(t)
	groups := []any{"items", "profiles", "images"}
	spec.Paths.Find("/items/{id}").Get.Extensions = map[string]any{"x-cli-group": groups}
	spec.Paths.Find("/items").Post.Extensions = map[string]any{"x-cli-group": groups}
	spec.Paths.Find("/public").Get.Extensions = map[string]any{"x-cli-group": "items"}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprintf(w, "%s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	out, _, err := runCommand(t, spec, server.URL, "", "items", "profiles", "images", "--help")
	if err != nil || !strings.Contains(out, "demo items profiles images [command]") ||
		!strings.Contains(out, "create") || !strings.Contains(out, "read") || requests.Load() != 0 {
		t.Fatalf("nested help: %v, requests %d, output %q", err, requests.Load(), out)
	}
	for _, test := range []struct {
		args []string
		body string
		want string
	}{
		{[]string{"items", "profiles", "images", "read", "--id", "1"}, "", "GET /items/1"},
		{[]string{"items", "profiles", "images", "create", "--body-stdin"}, `{"name":"Milo"}`, "POST /items"},
		{[]string{"items", "public"}, "", "GET /public"},
	} {
		args := append(test.args, "--output", "raw")
		out, stderr, err := runCommand(t, spec, server.URL, test.body, args...)
		if err != nil || stderr != "" || out != test.want {
			t.Fatalf("%v: got %q, stderr %q, error %v", args, out, stderr, err)
		}
	}
}

func TestNestedGroupValidation(t *testing.T) {
	unused := func(string, Input) (*http.Request, error) {
		t.Fatal("constructing commands must not build HTTP requests")
		return nil, nil
	}
	builders := map[string]Builder{"read": unused, "create": unused, "public": unused}
	for _, test := range []struct {
		name        string
		group       any
		leaf        string
		secondGroup any
		secondLeaf  string
		want        string
	}{
		{"wrong type", true, "create", "items", "read", "expected a string or a list of strings"},
		{"non-string segment", []any{"items", 42}, "create", "items", "read", "each path segment must be a string"},
		{"empty segment", []any{"items", ""}, "create", "items", "read", "nonempty kebab-case"},
		{"invalid segment", []any{"items", "invalid name"}, "create", "items", "read", "nonempty kebab-case"},
		{"reserved group", []any{"items", "help"}, "create", "items", "read", "reserved CLI group name"},
		{"duplicate leaf", []any{"items", "profiles"}, "read", []any{"items", "profiles"}, "read", "CLI command collision: demo items profiles read"},
		{"leaf before group", "items", "profiles", []any{"items", "profiles"}, "read", "CLI group collision: demo items profiles"},
		{"group before leaf", []any{"items", "profiles"}, "create", "items", "profiles", "CLI command collision: demo items profiles"},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := document(t)
			spec.Paths.Find("/items").Post.Extensions = map[string]any{"x-cli-group": test.group, "x-cli-name": test.leaf}
			spec.Paths.Find("/items/{id}").Get.Extensions = map[string]any{"x-cli-group": test.secondGroup, "x-cli-name": test.secondLeaf}
			_, err := New("demo", spec, builders)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestBearerPublicTLSAndBody(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "private-bearer")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/public" {
			if r.Header.Get("Authorization") != "" {
				t.Error("credentials sent to anonymous operation")
			}
		} else if r.Header.Get("Authorization") != "Bearer private-bearer" {
			t.Error("bearer missing")
		}
		if r.Method == "POST" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["name"] != "accepted" {
				t.Error("body changed")
			}
			w.WriteHeader(201)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()
	certificate, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCommand(t, document(t), server.URL, "", "items", "read", "--id", "1"); err == nil || ExitCode(err) != 3 {
		t.Fatalf("untrusted certificate accepted: %v", err)
	}
	for _, args := range [][]string{{"items", "read", "--id", "1"}, {"public"}, {"items", "create", "--body-stdin"}} {
		out, _, err := runCommand(t, document(t), server.URL, `{"name":"accepted"}`, append(args, "--ca-file", ca, "--output", "json")...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("TLS request: %v: %s", err, out)
		}
	}
}

func TestResponseErrorsRedirectsAndLimits(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "private-bearer")
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetCalls.Add(1); fmt.Fprint(w, `{}`) }))
	defer target.Close()
	for _, tc := range []struct {
		name, body   string
		status, code int
		args         []string
		want         string
	}{
		{"HTTP failure", `private-bearer echoed-error`, 403, 4, nil, ""},
		{"redirect", `private-bearer`, 302, 4, nil, ""},
		{"not modified", "", 304, 0, nil, ""},
		{"no content", "", 204, 0, nil, ""},
		{"JSON limit", `{"large":"private-bearer"}`, 200, 3, []string{"--max-output-bytes", "3"}, ""},
		{"not JSON", "raw-bytes", 200, 3, nil, ""},
		{"raw", "raw-bytes", 200, 0, []string{"--output", "raw"}, "raw-bytes"},
		{"raw limit", "abcdef", 200, 3, []string{"--output", "raw", "--max-output-bytes", "3"}, "abc"},
		{"ETag", `{}`, 200, 0, []string{"--response-header", "ETag"}, "\"etag\"\n"},
		{"ETag CSV", `{}`, 200, 0, []string{"--response-header", "ETag", "--output", "csv"}, "\"etag\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"etag"`)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			out, stderr, err := runCommand(t, document(t), server.URL, "", append([]string{"items", "read", "--id", "1"}, tc.args...)...)
			code := 0
			if err != nil {
				code = ExitCode(err)
			}
			if code != tc.code || out != tc.want {
				t.Fatalf("got code %d output %q error %v", code, out, err)
			}
			if strings.Contains(stderr, "private-bearer") || (err != nil && strings.Contains(err.Error(), "private-bearer")) {
				t.Fatal("credential leaked")
			}
		})
	}
	if targetCalls.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestAuthenticationAlternativesAndAND(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "bearer")
	t.Setenv("SERVICE_KEY", "api-key")
	t.Setenv("SERVICE_BASIC", "user:password")
	spec := document(t)
	spec.Components.SecuritySchemes["apiKey"] = &openapi3.SecuritySchemeRef{Value: &openapi3.SecurityScheme{Type: "apiKey", In: "header", Name: "X-API-Key"}}
	spec.Components.SecuritySchemes["basic"] = &openapi3.SecuritySchemeRef{Value: &openapi3.SecurityScheme{Type: "http", Scheme: "basic"}}
	spec.Security = openapi3.SecurityRequirements{{"apiKey": {}, "bearerAuth": {}}}
	req, _ := http.NewRequest("GET", "https://example.invalid", nil)
	if applied, err := authenticate(req, spec, spec.Paths.Value("/items/{id}").Get, "DEMO", []string{"apiKey=SERVICE_KEY"}, false); err != nil || !applied {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer bearer" || req.Header.Get("X-API-Key") != "api-key" {
		t.Fatal("AND requirements not applied")
	}
	spec.Security = openapi3.SecurityRequirements{{"basic": {}}}
	req.Header = http.Header{}
	if applied, err := authenticate(req, spec, spec.Paths.Value("/items/{id}").Get, "DEMO", []string{"basic=SERVICE_BASIC"}, false); err != nil || !applied {
		t.Fatal(err)
	}
	user, password, ok := req.BasicAuth()
	if !ok || user != "user" || password != "password" {
		t.Fatal("basic authentication mismatch")
	}
	spec.Security = openapi3.SecurityRequirements{{"apiKey": {}}, {"bearerAuth": {}}}
	req.Header = http.Header{}
	if applied, err := authenticate(req, spec, spec.Paths.Value("/items/{id}").Get, "DEMO", nil, false); err != nil || !applied || req.Header.Get("Authorization") != "Bearer bearer" {
		t.Fatal("OR fallback failed")
	}
	if req.Header.Get("X-API-Key") != "" {
		t.Fatal("partial OR credential leaked")
	}
}

func TestTimeoutAndHTTPPolicy(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "private-bearer")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	started := time.Now()
	_, _, err := runCommand(t, document(t), server.URL, "", "items", "read", "--id", "1", "--timeout", "40ms")
	if err == nil || ExitCode(err) != 3 || time.Since(started) > 2*time.Second {
		t.Fatalf("timeout failed: %v", err)
	}
	for _, raw := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com?token=secret", "file:///tmp/foo"} {
		if _, err := serverURL(raw, false); err == nil {
			t.Fatalf("unsafe server accepted: %s", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "https://example.com/prefix"} {
		if _, err := serverURL(raw, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := serverURL("http://example.com", true); err != nil {
		t.Fatal(err)
	}
}

func TestIntegerPrecision(t *testing.T) {
	spec := document(t)
	p := spec.Paths.Value("/items/{id}").Parameters[0].Value
	raw, err := parameterJSON(p, &parameterValue{kind: "integer", values: []string{"9007199254740993"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Values: map[string]json.RawMessage{"path:id": raw}}
	var id int64
	if err := input.Decode("path", "id", &id); err != nil || id != 9007199254740993 {
		t.Fatalf("integer precision lost: %d %v", id, err)
	}
}

func TestCommandHelpUsesOverlayText(t *testing.T) {
	doc := document(t)
	op := doc.Paths.Find("/public").Get
	op.Description = "Contract implementation details."
	op.Extensions = map[string]any{
		"x-cli-description": "Show the public result.",
		"x-cli-example":     "demo public --output json",
	}
	out, _, err := runCommand(t, doc, "", "", "public", "--help")
	if err != nil || !strings.Contains(out, "Show the public result.") ||
		!strings.Contains(out, "demo public --output json") || strings.Contains(out, op.Description) {
		t.Fatalf("help = %q, err = %v", out, err)
	}
}

func TestEventStreamDefaultsToRaw(t *testing.T) {
	const frame = "id: 1\ndata: ready\n\n"
	var hold atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fmt.Fprint(w, frame)
		w.(http.Flusher).Flush()
		if hold.Load() {
			<-r.Context().Done()
		}
	}))
	defer server.Close()

	doc := document(t)
	out, _, err := runCommand(t, doc, server.URL, "", "public")
	if err == nil || ExitCode(err) != 3 || out != "" {
		t.Fatalf("undeclared stream output = %q, err = %v", out, err)
	}
	doc.Paths.Find("/public").Get.Responses.Status(200).Value.Content = openapi3.Content{
		"text/event-stream": {},
	}
	out, _, err = runCommand(t, doc, server.URL, "", "public")
	if err != nil || out != frame {
		t.Fatalf("default output = %q, err = %v", out, err)
	}
	out, _, err = runCommand(t, doc, server.URL, "", "public", "--output", "table")
	if err == nil || ExitCode(err) != 3 || out != "" {
		t.Fatalf("explicit table output = %q, err = %v", out, err)
	}
	hold.Store(true)
	out, _, err = runCommand(t, doc, server.URL, "", "public", "--timeout", "1s")
	if err == nil || ExitCode(err) != 3 || err.Error() != "request timed out" || out != frame {
		t.Fatalf("open stream output = %q, err = %v", out, err)
	}
}

func TestHTTPProblemDisplaysOnlyDeclaredDiagnosticFields(t *testing.T) {
	doc := document(t)
	op := doc.Paths.Find("/public").Get
	schema := openapi3.NewObjectSchema().WithProperty("code", openapi3.NewStringSchema().WithEnum("state_conflict", "idempotency_conflict", "unsafe\ncode"))
	documentation := openapi3.NewStringSchema()
	documentation.Format = "uri"
	schema.WithProperty("documentation", documentation)
	op.Responses.Set("409", &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription("Conflict").WithContent(
		openapi3.Content{"application/problem+json": {Schema: &openapi3.SchemaRef{Value: schema}}},
	)})
	const problemJSON = "application/problem+json; charset=utf-8"
	for _, tc := range []struct {
		name, media, body, suffix string
		limits                    []string
	}{
		{"state conflict with documentation", problemJSON, `{"code":"state_conflict","detail":"secret","documentation":"https://docs.example.com/problems#state_conflict"}`,
			" (state_conflict)\nDocumentation: https://docs.example.com/problems#state_conflict", nil},
		{"wrong link type", problemJSON, `{"code":"state_conflict","documentation":123}`, " (state_conflict)", nil},
		{"non-HTTPS link", problemJSON, `{"code":"state_conflict","documentation":"http://docs.example.com/problems"}`, " (state_conflict)", nil},
		{"relative link", problemJSON, `{"code":"state_conflict","documentation":"/problems"}`, " (state_conflict)", nil},
		{"link credentials", problemJSON, `{"code":"state_conflict","documentation":"https://user:secret@docs.example.com/problems"}`, " (state_conflict)", nil},
		{"link query", problemJSON, `{"code":"state_conflict","documentation":"https://docs.example.com/problems?token=secret"}`, " (state_conflict)", nil},
		{"link control character", problemJSON, `{"code":"state_conflict","documentation":"https://docs.example.com/problems#\u001b[0m"}`, " (state_conflict)", nil},
		{"idempotency conflict", problemJSON, `{"code":"idempotency_conflict"}`, " (idempotency_conflict)", nil},
		{"other media", "application/json", `{"code":"state_conflict"}`, "", nil},
		{"undeclared code", problemJSON, `{"code":"secret"}`, "", nil},
		{"control character", problemJSON, `{"code":"unsafe\ncode"}`, "", nil},
		{"malformed body", problemJSON, `{"code":"state_conflict"`, "", nil},
		{"oversize body", problemJSON, `{"code":"state_conflict"}`, "", []string{"--max-output-bytes", "8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.media)
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			out, _, err := runCommand(t, doc, server.URL, "", append([]string{"public"}, tc.limits...)...)
			if err == nil || ExitCode(err) != 4 || err.Error() != "HTTP 409 Conflict"+tc.suffix || out != "" {
				t.Fatalf("stdout = %q, err = %v", out, err)
			}
		})
	}
}

func TestHTTPProblemOmitsDocumentationOutsideContract(t *testing.T) {
	doc := document(t)
	op := doc.Paths.Find("/public").Get
	schema := openapi3.NewObjectSchema().WithProperty("code", openapi3.NewStringSchema().WithEnum("state_conflict"))
	op.Responses.Set("409", &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription("Conflict").WithContent(
		openapi3.Content{"application/problem+json": {Schema: &openapi3.SchemaRef{Value: schema}}},
	)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"code":"state_conflict","documentation":"https://docs.example.com/problems"}`)
	}))
	defer server.Close()
	out, _, err := runCommand(t, doc, server.URL, "", "public")
	if err == nil || ExitCode(err) != 4 || err.Error() != "HTTP 409 Conflict (state_conflict)" || out != "" {
		t.Fatalf("stdout = %q, err = %v", out, err)
	}
}

func TestHTTPFailureJSON(t *testing.T) {
	doc := document(t)
	op := doc.Paths.Find("/public").Get
	link := openapi3.NewStringSchema()
	link.Format = "uri"
	schema := openapi3.NewObjectSchema().
		WithProperty("code", openapi3.NewStringSchema().WithEnum("state_conflict")).
		WithProperty("type", openapi3.NewStringSchema()).
		WithProperty("title", openapi3.NewStringSchema()).
		WithProperty("documentation", link)
	op.Responses.Set("409", &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription("Conflict").WithContent(
		openapi3.Content{"application/problem+json": {Schema: &openapi3.SchemaRef{Value: schema}}},
	)})
	const problemJSON = "application/problem+json"
	for _, tc := range []struct {
		name, media, body, problemType, title, code, documentation string
	}{
		{
			"declared problem", problemJSON,
			`{"type":"urn:example:state-conflict","title":"State conflict","status":400,"code":"state_conflict","documentation":"https://docs.example.com/state","detail":"private","currentMessage":{"text":"private"}}`,
			"urn:example:state-conflict", "State conflict", "state_conflict", "https://docs.example.com/state",
		},
		{"standard problem", problemJSON, `{"type":"urn:example:standard","title":"Standard problem"}`, "urn:example:standard", "Standard problem", "", ""},
		{"relative type", problemJSON, `{"type":"/problems/conflict","title":"Relative problem"}`, "/problems/conflict", "Relative problem", "", ""},
		{"HTML response", "text/html", "<p>private</p>", "about:blank", "Conflict", "", ""},
		{"invalid JSON", problemJSON, "{", "about:blank", "Conflict", "", ""},
		{"undeclared code", problemJSON, `{"code":"private","title":"private"}`, "about:blank", "Conflict", "", ""},
		{"invalid optional fields", problemJSON, `{"type":123,"title":{},"code":"state_conflict","documentation":"https://docs.example.com/?token=private"}`, "about:blank", "Conflict", "state_conflict", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.media)
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()

			if tc.name == "standard problem" {
				code := schema.Properties["code"]
				delete(schema.Properties, "code")
				defer func() { schema.Properties["code"] = code }()
			}
			out, _, err := runCommand(t, doc, server.URL, "", "public", "--output", "json")
			if out != "" || err == nil || ExitCode(err) != 4 {
				t.Fatalf("stdout = %q, err = %v", out, err)
			}
			var got httpProblem
			if err := json.Unmarshal([]byte(err.Error()), &got); err != nil {
				t.Fatal(err)
			}
			want := httpProblem{Type: tc.problemType, Title: tc.title, Status: 409, Code: tc.code, Documentation: tc.documentation}
			if strings.HasPrefix(want.Type, "/") {
				want.Type = server.URL + want.Type
			}
			if got != want || strings.Contains(err.Error(), "private") {
				t.Fatalf("diagnostic = %s, want %+v", err, want)
			}
		})
	}
}

func TestHTTPProblemCatalogKeepsRequestDataOutOfDiagnostics(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "fixture-private-token")
	doc := document(t)
	op := doc.Paths.Find("/items").Post
	schema := openapi3.NewObjectSchema().WithProperty("code", openapi3.NewStringSchema().WithEnum("state_conflict"))
	link := openapi3.NewStringSchema()
	link.Format = "uri"
	schema.WithProperty("documentation", link)
	op.Responses.Set("409", &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription("Conflict").WithContent(
		openapi3.Content{"application/problem+json": {Schema: &openapi3.SchemaRef{Value: schema}}},
	)})
	const marker = "fixture-private-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+marker {
			t.Error("fixture did not receive the credential")
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `{"type":"urn:example:%s","title":"%s","code":"state_conflict","documentation":"https://docs.example.com/%s"}`, marker, marker, marker)
	}))
	defer server.Close()
	for _, catalog := range []bool{false, true} {
		if catalog {
			schema.Extensions = map[string]any{"x-cli-problem-catalog": map[string]any{
				"type-prefix": "urn:example:problem:", "documentation-base": "https://docs.example.com/catalog",
				"titles": map[string]any{"state_conflict": "State conflict"},
			}}
		}
		for _, format := range []string{"table", "json"} {
			out, _, err := runCommand(t, doc, server.URL, `{"name":"fixture-private-token"}`, "items", "create", "--body-stdin", "--output", format)
			if err == nil || ExitCode(err) != 4 || out != "" || strings.Contains(err.Error(), marker) ||
				strings.Contains(err.Error(), "https://docs.example.com/catalog#state_conflict") != catalog {
				t.Fatalf("catalog=%t format=%s stdout=%q err=%v", catalog, format, out, err)
			}
		}
	}
}

func TestHTTPProblemBodyDeadlineIsTransportFailure(t *testing.T) {
	doc := document(t)
	doc.Paths.Find("/public").Get.Responses.Set("503", &openapi3.ResponseRef{Value: openapi3.NewResponse().WithDescription("Unavailable").WithContent(
		openapi3.Content{"application/problem+json": {}},
	)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	out, _, err := runCommand(t, doc, server.URL, "", "public", "--output", "json", "--timeout", "250ms")
	if err == nil || ExitCode(err) != 3 || err.Error() != "request timed out" || out != "" {
		t.Fatalf("stdout = %q, err = %v", out, err)
	}
}

func TestDeprecatedOperationAndMissingHeaderKeepJSONDiagnosticsClean(t *testing.T) {
	doc := document(t)
	doc.Paths.Find("/public").Get.Deprecated = true
	var failHTTP atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failHTTP.Load() {
			w.WriteHeader(http.StatusConflict)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	for _, tc := range []struct {
		args     []string
		exitCode int
	}{
		{[]string{"public", "--output", "json", "--unknown"}, 2},
		{[]string{"public", "--output", "json", "--response-header", "ETag"}, 3},
		{[]string{"public", "--output", "json"}, 4},
	} {
		failHTTP.Store(tc.exitCode == 4)
		out, stderr, err := runCommand(t, doc, server.URL, "", tc.args...)
		if err == nil || ExitCode(err) != tc.exitCode || out != "" || stderr != "" {
			t.Fatalf("args=%v stdout=%q stderr=%q err=%v", tc.args, out, stderr, err)
		}
	}
	failHTTP.Store(false)
	out, stderr, err := runCommand(t, doc, server.URL, "", "public")
	if err != nil || !strings.Contains(stderr, "deprecated") || out == "" {
		t.Fatalf("text invocation stdout=%q stderr=%q err=%v", out, stderr, err)
	}
}
