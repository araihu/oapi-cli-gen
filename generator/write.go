package generator

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

const manifestName = ".oapi-cli-gen.json"

func managedPath(name string) bool {
	if name == "internal/api/client.gen.go" || name == "internal/cli/commands.gen.go" {
		return true
	}
	for _, file := range []string{"runtime.gen.go", "auth.gen.go", "input.gen.go", "output.gen.go"} {
		if name == "internal/cliruntime/"+file {
			return true
		}
	}
	parts := strings.Split(name, "/")
	return len(parts) == 3 && parts[0] == "cmd" && commandName.MatchString(parts[1]) && parts[2] == "main.go"
}

type manifest struct {
	Format      int               `json:"format"`
	Module      string            `json:"module"`
	SpecSHA256  string            `json:"specSHA256"`
	OAPICodegen string            `json:"oapiCodegen"`
	Files       map[string]string `json:"files"`
}

func checksum(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func (r *Result) metadata() ([]byte, error) {
	m := manifest{1, r.Module, r.SpecHash, OAPIVersion, map[string]string{}}
	for name, data := range r.Files {
		if name != "go.mod" {
			m.Files[name] = checksum(data)
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	return append(data, '\n'), err
}

func (r *Result) checkModule(data []byte) error {
	file, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return err
	}
	if file.Module == nil || file.Module.Mod.Path != r.Module {
		return errors.New("output go.mod belongs to another module")
	}
	if file.Go == nil || semver.Compare("v"+file.Go.Version, "v"+GoVersion) < 0 {
		return fmt.Errorf("output module requires Go %s or newer", GoVersion)
	}
	versions := map[string]string{}
	for _, req := range file.Require {
		versions[req.Mod.Path] = req.Mod.Version
	}
	for _, replacement := range file.Replace {
		if _, protected := r.Requires[replacement.Old.Path]; protected {
			return fmt.Errorf("output go.mod replaces protected dependency %s; remove the replacement before regeneration", replacement.Old.Path)
		}
	}
	for path, version := range r.Requires {
		if semver.Compare(versions[path], version) < 0 {
			return fmt.Errorf("output go.mod needs %s@%s or newer; update dependencies before regeneration", path, version)
		}
	}
	return nil
}

// Check rejects generated drift without writing any files. go.mod may be tidied
// or extended, provided required dependencies are at least the generated floors.
func (r *Result) Check(directory string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	for name, expected := range r.Files {
		actual, err := root.ReadFile(name)
		if err != nil {
			return fmt.Errorf("generated drift: %s: %w", name, err)
		}
		if name == "go.mod" {
			if err := r.checkModule(actual); err != nil {
				return err
			}
			continue
		}
		if checksum(actual) != checksum(expected) {
			return fmt.Errorf("generated drift: %s", name)
		}
	}
	expected, err := r.metadata()
	if err != nil {
		return err
	}
	actual, err := root.ReadFile(manifestName)
	if err != nil || checksum(actual) != checksum(expected) {
		return errors.New("generated drift: manifest")
	}
	return nil
}

// Write creates a new output or updates only unchanged generator-owned files.
// Each file is published atomically; user files and go.mod/go.sum are preserved.
func (r *Result) Write(directory string, update bool) error {
	if !update {
		if _, err := os.Stat(directory); !errors.Is(err, fs.ErrNotExist) {
			return errors.New("output already exists; use a new directory or --update for a managed project")
		}
		parent := filepath.Dir(directory)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return err
		}
		staging, err := os.MkdirTemp(parent, ".oapi-cli-gen-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		if err := r.writeFiles(staging, nil); err != nil {
			return err
		}
		return os.Rename(staging, directory)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	data, err := root.ReadFile(manifestName)
	if err != nil {
		root.Close()
		return errors.New("--update requires an existing oapi-cli-gen manifest")
	}
	var previous manifest
	if err := json.Unmarshal(data, &previous); err != nil {
		root.Close()
		return errors.New("invalid output manifest")
	}
	if previous.Format != 1 || previous.Module != r.Module {
		root.Close()
		return errors.New("output manifest belongs to another generator format or module")
	}
	for name, expected := range previous.Files {
		if !fs.ValidPath(name) || !managedPath(name) {
			root.Close()
			return errors.New("invalid generated file in output manifest")
		}
		actual, err := root.ReadFile(name)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			root.Close()
			return err
		}
		if err == nil && checksum(actual) != expected {
			root.Close()
			return fmt.Errorf("refusing to replace edited generated file %s", name)
		}
	}
	mod, err := root.ReadFile("go.mod")
	root.Close()
	if err != nil {
		return err
	}
	if err := r.checkModule(mod); err != nil {
		return err
	}
	return r.writeFiles(directory, &previous)
}

func (r *Result) writeFiles(directory string, previous *manifest) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	paths := make([]string, 0, len(r.Files))
	for name := range r.Files {
		paths = append(paths, name)
	}
	slices.Sort(paths)
	for _, name := range paths {
		if previous != nil && name == "go.mod" {
			continue
		}
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		if err := atomicWrite(root, name, r.Files[name]); err != nil {
			return err
		}
	}
	if previous != nil {
		for name := range previous.Files {
			if _, exists := r.Files[name]; !exists {
				if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
	}
	data, err := r.metadata()
	if err != nil {
		return err
	}
	return atomicWrite(root, manifestName, data)
}

func atomicWrite(root *os.Root, name string, data []byte) error {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(name), ".oapi-"+hex.EncodeToString(nonce[:]))
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return root.Rename(temporary, name)
}
