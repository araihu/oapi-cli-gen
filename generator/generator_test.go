package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func fixture(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(filepath.Join("..", "testdata", path))
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}
func petOptions(t *testing.T) Options {
	return Options{Spec: fixture(t, "pets/openapi.yaml"), Name: "pets", Module: "example.com/pets"}
}

func generated(t *testing.T, options Options) *Result {
	t.Helper()
	result, err := Generate(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func goCommand(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "go", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=auto")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, output)
	}
}

func buildProject(t *testing.T, options Options) (string, string) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "project")
	result := generated(t, options)
	if err := result.Write(directory, false); err != nil {
		t.Fatal(err)
	}
	goCommand(t, directory, "mod", "tidy")
	if err := result.Check(directory); err != nil {
		t.Fatalf("tidy should preserve the dependency policy: %v", err)
	}
	binary := filepath.Join(t.TempDir(), options.Name)
	goCommand(t, directory, "build", "-o", binary, "./cmd/"+options.Name)
	return directory, binary
}

func TestGeneratedClientRunsSDKSerialization(t *testing.T) {
	_, binary := buildProject(t, petOptions(t))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/base/health" {
			if r.Header.Get("Authorization") != "" {
				t.Error("public operation received credentials")
			}
			fmt.Fprint(w, `{}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("bearer credential missing")
		}
		if r.Method == "GET" {
			if r.URL.Path != "/base/pets/9007199254740993" {
				t.Errorf("path parameter corrupted: %s", r.URL.Path)
			}
			query := r.URL.Query()
			if query.Get("limit") != "3" || query.Get("server") != "parameter-value" || query.Get("date") != "2026-10-25" {
				t.Errorf("typed query serialization failed: %v", query)
			}
			if values := query["tag"]; len(values) != 2 || values[0] != "a/b ?" || values[1] != "second" {
				t.Error("array query encoding failed")
			}
			if r.Header.Get("If-Match") != `"v1"` {
				t.Error("conditional header missing")
			}
		} else {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["name"] != "Moco" {
				t.Error("JSON body changed")
			}
			w.WriteHeader(201)
		}
		fmt.Fprint(w, `{"accepted":true}`)
	}))
	defer server.Close()
	run := func(body string, args ...string) (string, string, error) {
		command := exec.CommandContext(t.Context(), binary, append([]string{"--server", server.URL + "/base"}, args...)...)
		command.Env = append(os.Environ(), "PETS_TOKEN=test-token")
		command.Stdin = strings.NewReader(body)
		var out, stderr bytes.Buffer
		command.Stdout = &out
		command.Stderr = &stderr
		err := command.Run()
		return out.String(), stderr.String(), err
	}
	if _, _, err := run("", "--help"); err != nil || calls.Load() != 0 {
		t.Fatal("help failed or sent requests")
	}
	out, stderr, err := run("", "pets", "get", "--pet-id", "9007199254740993", "--tag", "a/b ?", "--tag", "second", "--if-match", `"v1"`, "--query-server", "parameter-value", "--date", "2026-10-25")
	if err != nil || !strings.Contains(out, `"accepted": true`) {
		t.Fatalf("get: %v %s %s", err, out, stderr)
	}
	if _, stderr, err := run(`{"name":"Moco","species":"cat"}`, "pets", "create", "--body-stdin"); err != nil {
		t.Fatalf("create: %v %s", err, stderr)
	}
	if _, stderr, err := run("", "health"); err != nil {
		t.Fatalf("health: %v %s", err, stderr)
	}
	before := calls.Load()
	for _, args := range [][]string{{"pets", "get"}, {"pets", "get", "--pet-id", "1", "--limit", "99"}, {"pets", "create", "--body-stdin"}, {"pets", "get", "--pet-id", "1", "--date", "bad-date"}} {
		out, stderr, err := run(`{"name":"private-body","species":"bird"}`, args...)
		var exit *exec.ExitError
		if err == nil {
			t.Fatal("invalid request accepted")
		}
		if typed, ok := err.(*exec.ExitError); ok {
			exit = typed
		}
		if exit == nil || exit.ExitCode() != 2 || strings.Contains(stderr, "private-body") || strings.Contains(stderr, "test-token") || out != "" {
			t.Fatalf("unsafe error: %v stdout=%s stderr=%s", err, out, stderr)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid input reached the server")
	}
}

func TestMocoContractsGenerateCompilableIndependentClients(t *testing.T) {
	for _, name := range []string{"public", "internal"} {
		t.Run(name, func(t *testing.T) {
			_, binary := buildProject(t, Options{Spec: fixture(t, "moco/"+name+".yaml"), Name: "moco-" + name, Module: "example.com/moco-" + name})
			command := exec.CommandContext(t.Context(), binary, "--help")
			data, err := command.CombinedOutput()
			if err != nil || !bytes.Contains(data, []byte("Available Commands:")) {
				t.Fatalf("generated help: %v %s", err, data)
			}
		})
	}
}

func TestDeterministicConcurrentGeneration(t *testing.T) {
	options := petOptions(t)
	expected := generated(t, options)
	var wait sync.WaitGroup
	for range 3 {
		wait.Go(func() {
			actual, err := Generate(t.Context(), options)
			if err != nil {
				t.Error(err)
				return
			}
			for name, data := range expected.Files {
				if !bytes.Equal(data, actual.Files[name]) {
					t.Errorf("nondeterministic %s", name)
				}
			}
		})
	}
	wait.Wait()
	for name, data := range expected.Files {
		if strings.Contains(string(data), filepath.Dir(options.Spec)) {
			t.Errorf("machine path leaked into %s", name)
		}
	}
}

func TestRegenerationProtectsUserFilesAndGeneratedEdits(t *testing.T) {
	result := generated(t, petOptions(t))
	directory := filepath.Join(t.TempDir(), "project")
	if err := result.Write(directory, false); err != nil {
		t.Fatal(err)
	}
	if err := result.Write(directory, false); err == nil {
		t.Fatal("existing output overwritten")
	}
	custom := filepath.Join(directory, "custom.go")
	if err := os.WriteFile(custom, []byte("package custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := result.Write(directory, true); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(custom); err != nil || string(data) != "package custom\n" {
		t.Fatal("user file changed")
	}
	managed := filepath.Join(directory, "internal/cli/commands.gen.go")
	if err := os.WriteFile(managed, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := result.Check(directory); err == nil {
		t.Fatal("generated drift not detected")
	}
	if err := result.Write(directory, true); err == nil {
		t.Fatal("edited generated file overwritten")
	}
}

func TestRemoteAndEscapingReferencesAreNeverFetched(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"type":"string"}`) }))
	defer server.Close()
	for _, reference := range []string{server.URL + "/schema", "../outside.yaml", "outside-link.yaml"} {
		t.Run(reference, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "outside.yaml"), []byte("type: string"), 0o600); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(directory, "spec")
			if err := os.Mkdir(base, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(directory, "outside.yaml"), filepath.Join(base, "outside-link.yaml")); err != nil {
				t.Fatal(err)
			}
			data := fmt.Sprintf(`{"openapi":"3.0.3","info":{"title":"x","version":"1"},"paths":{"/x":{"get":{"operationId":"getX","responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"$ref":%q}}}}}}}}}`, reference)
			filename := filepath.Join(base, "openapi.json")
			if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Generate(t.Context(), Options{Spec: filename, Name: "test", Module: "example.com/test"}); err == nil {
				t.Fatal("unsafe reference accepted")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("untrusted reference was fetched")
	}
}

func TestOpenAPI31AndNames(t *testing.T) {
	directory := t.TempDir()
	filename := filepath.Join(directory, "api.json")
	data := `{"openapi":"3.1.0","info":{"title":"3.1 test","version":"1"},"paths":{"/hello":{"get":{"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":["string","null"]}}}}}}}}}`
	if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Generate(context.Background(), Options{Spec: filename, Name: "hello", Module: "example.com/hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(result.Files["internal/cli/commands.gen.go"], []byte("GetHello")) {
		t.Fatal("missing operationId was not derived")
	}
	buildProject(t, Options{Spec: filename, Name: "hello", Module: "example.com/hello"})
	for _, name := range []string{"../escape", "bad name", "name;exit", "UPPER"} {
		if _, err := Generate(t.Context(), Options{Spec: filename, Name: name, Module: "example.com/test"}); err == nil {
			t.Fatalf("invalid name accepted: %s", name)
		}
	}
}

func TestDependencyFloorsRejectDowngradesAndReplacements(t *testing.T) {
	options := petOptions(t)
	options.Requires = map[string]string{"golang.org/x/text": "v0.32.0"}
	if _, err := Generate(t.Context(), options); err == nil {
		t.Fatal("vulnerable dependency downgrade allowed")
	}
	options.Requires = nil
	result := generated(t, options)
	directory := filepath.Join(t.TempDir(), "project")
	if err := result.Write(directory, false); err != nil {
		t.Fatal(err)
	}
	mod := append([]byte{}, result.Files["go.mod"]...)
	mod = append(mod, []byte("\nreplace golang.org/x/text => golang.org/x/text v0.32.0\n")...)
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), mod, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := result.Check(directory); err == nil {
		t.Fatal("replacement bypassed dependency floors")
	}
}

func TestManifestCannotDeleteUnmanagedFiles(t *testing.T) {
	result := generated(t, petOptions(t))
	directory := filepath.Join(t.TempDir(), "project")
	if err := result.Write(directory, false); err != nil {
		t.Fatal(err)
	}
	userData := []byte("operator-owned")
	if err := os.WriteFile(filepath.Join(directory, "user.txt"), userData, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var modified manifest
	if err := json.Unmarshal(data, &modified); err != nil {
		t.Fatal(err)
	}
	modified.Files["user.txt"] = checksum(userData)
	data, err = json.Marshal(modified)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, manifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := result.Write(directory, true); err == nil {
		t.Fatal("unsafe manifest accepted")
	}
	actual, err := os.ReadFile(filepath.Join(directory, "user.txt"))
	if err != nil || !bytes.Equal(actual, userData) {
		t.Fatal("user file changed")
	}
}
