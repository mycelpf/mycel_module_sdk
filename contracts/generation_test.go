package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// SDK-T1 (sdk.md S1): verification fails on a changed proto with stale bindings, a missing
// generator, a wrong generator version and an unexpected generated file, and it never
// changes the checkout.

// repositoryRoot is the module root, one level above this package.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// copyFixture copies the parts of the repository the generation check reads into a fresh
// directory, so each scenario can mutate its own copy.
func copyFixture(t *testing.T) string {
	t.Helper()
	source := repositoryRoot(t)
	target := t.TempDir()
	for _, directory := range []string{"_scripts", "contracts"} {
		err := filepath.WalkDir(filepath.Join(source, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, _ := filepath.Rel(source, path)
			destination := filepath.Join(target, relative)
			if entry.IsDir() {
				return os.MkdirAll(destination, 0o755)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.WriteFile(destination, data, info.Mode().Perm())
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return target
}

// treeHash is a digest of every file path and content under root.
func treeHash(t *testing.T, root string) string {
	t.Helper()
	var names []string
	contents := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		names = append(names, relative)
		contents[relative] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		digest.Write([]byte(name))
		digest.Write(contents[name])
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// pinnedToolsAvailable reports whether the real generators at the pinned versions are on
// PATH. Scenarios that need real generation skip without them; the scenarios about
// missing and wrongly versioned tools use fake tools and always run.
func pinnedToolsAvailable(t *testing.T) bool {
	t.Helper()
	command := exec.Command("bash", filepath.Join(repositoryRoot(t), "_scripts", "generate.sh"), t.TempDir())
	return command.Run() == nil
}

func runCheck(t *testing.T, root string, path string) (string, error) {
	t.Helper()
	command := exec.Command("bash", filepath.Join(root, "_scripts", "check_generated.sh"), root)
	if path != "" {
		command.Env = append(os.Environ(), "PATH="+path)
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

// toolDirectory builds a PATH directory holding the system tools the scripts use and the
// given fake generators (name → script body). A generator absent from fakes is absent.
func toolDirectory(t *testing.T, fakes map[string]string) string {
	t.Helper()
	directory := t.TempDir()
	for _, tool := range []string{"bash", "find", "sort", "grep", "sed", "cmp", "mktemp", "dirname", "head", "rm", "cat", "mkdir", "env"} {
		resolved, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("system tool %s not available: %v", tool, err)
		}
		if err := os.Symlink(resolved, filepath.Join(directory, tool)); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func TestGenerationCheckPassesOnTheCheckoutAndDoesNotChangeIt(t *testing.T) {
	if !pinnedToolsAvailable(t) {
		t.Skip("pinned protoc and plugins are not on PATH (contracts/tooling.versions)")
	}
	root := repositoryRoot(t)
	before := treeHash(t, filepath.Join(root, "contracts"))
	if output, err := runCheck(t, root, ""); err != nil {
		t.Fatalf("check failed on the checkout: %v\n%s", err, output)
	}
	if after := treeHash(t, filepath.Join(root, "contracts")); after != before {
		t.Error("the generation check changed the checkout")
	}
}

func TestGenerationCheckFailsOnAChangedProtoWithStaleBindings(t *testing.T) {
	if !pinnedToolsAvailable(t) {
		t.Skip("pinned protoc and plugins are not on PATH (contracts/tooling.versions)")
	}
	root := copyFixture(t)
	proto := filepath.Join(root, "contracts", "modulerpc", "v1", "modulerpc.proto")
	data, err := os.ReadFile(proto)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), "message DescribeRequest {}", "message DescribeRequest { string probe_field_for_test = 1; }", 1)
	if changed == string(data) {
		t.Fatal("fixture mutation did not apply")
	}
	if err := os.WriteFile(proto, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeHash(t, root)
	output, err := runCheck(t, root, "")
	if err == nil {
		t.Fatalf("check passed with stale bindings:\n%s", output)
	}
	if !strings.Contains(output, "stale generated file") {
		t.Errorf("failure does not name the stale file:\n%s", output)
	}
	if treeHash(t, root) != before {
		t.Error("the generation check changed the checkout")
	}
}

func TestGenerationCheckFailsOnAnUnexpectedGeneratedFile(t *testing.T) {
	if !pinnedToolsAvailable(t) {
		t.Skip("pinned protoc and plugins are not on PATH (contracts/tooling.versions)")
	}
	root := copyFixture(t)
	stray := filepath.Join(root, "contracts", "modulerpc", "v1", "stray.pb.go")
	if err := os.WriteFile(stray, []byte("// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage modulerpc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := runCheck(t, root, "")
	if err == nil {
		t.Fatalf("check passed with an unexpected generated file:\n%s", output)
	}
	if !strings.Contains(output, "unexpected generated file contracts/modulerpc/v1/stray.pb.go") {
		t.Errorf("failure does not name the unexpected file:\n%s", output)
	}
}

func TestGenerationCheckFailsWhenAGeneratorIsMissing(t *testing.T) {
	root := copyFixture(t)
	path := toolDirectory(t, map[string]string{
		"protoc":             `echo "libprotoc 36.2"`,
		"protoc-gen-go-grpc": `echo "protoc-gen-go-grpc 1.6.2"`,
	})
	before := treeHash(t, root)
	output, err := runCheck(t, root, path)
	if err == nil {
		t.Fatalf("check passed without protoc-gen-go:\n%s", output)
	}
	if !strings.Contains(output, "protoc-gen-go is not installed") {
		t.Errorf("failure does not name the missing generator:\n%s", output)
	}
	if treeHash(t, root) != before {
		t.Error("the generation check changed the checkout")
	}
}

func TestGenerationCheckFailsOnAWrongGeneratorVersion(t *testing.T) {
	root := copyFixture(t)
	path := toolDirectory(t, map[string]string{
		"protoc":             `echo "libprotoc 36.2"`,
		"protoc-gen-go":      `echo "protoc-gen-go v1.0.0"`,
		"protoc-gen-go-grpc": `echo "protoc-gen-go-grpc 1.6.2"`,
	})
	output, err := runCheck(t, root, path)
	if err == nil {
		t.Fatalf("check passed with a wrong generator version:\n%s", output)
	}
	if !strings.Contains(output, `protoc-gen-go reports "protoc-gen-go v1.0.0"`) {
		t.Errorf("failure does not name the wrong version:\n%s", output)
	}
}
