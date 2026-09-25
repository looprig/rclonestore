package examples_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const offlineExamplesCommand = "GOWORK=off go test -race ./... -p=1 -parallel=1"

type examplesManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Repository    string `json:"repository"`
	ProofSources  []struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Path   string `json:"path"`
		Symbol string `json:"symbol"`
	} `json:"proofSources"`
	Examples []struct {
		ID             string            `json:"id"`
		Ecosystem      string            `json:"ecosystem"`
		Owner          string            `json:"owner"`
		SourcePath     string            `json:"sourcePath"`
		Availability   string            `json:"availability"`
		Versions       map[string]string `json:"versions"`
		OfflineCommand string            `json:"offlineCommand"`
		Assertion      string            `json:"assertion"`
		WorkflowPath   string            `json:"workflowPath"`
		JobID          string            `json:"jobId"`
		Cleanup        string            `json:"cleanup"`
		LiveGate       json.RawMessage   `json:"liveGate"`
		ProofIDs       []string          `json:"proofIds"`
	} `json:"examples"`
}

func TestDocsExamplesArtifacts(t *testing.T) {
	repositoryRoot := filepath.Clean("..")
	manifestData, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata/docs/examples.json"))
	if err != nil {
		t.Fatalf("read examples manifest: %v", err)
	}

	var manifest examplesManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("decode examples manifest: %v", err)
	}
	if manifest.SchemaVersion != 1 || manifest.Repository != "rclonestore" {
		t.Fatalf("manifest identity = schema %d repository %q", manifest.SchemaVersion, manifest.Repository)
	}

	expectedProofs := map[string]struct {
		proofType string
		path      string
		symbol    string
	}{
		"example-rclonestore-blob-remote-fixture": {
			proofType: "executable-fixture",
			path:      "examples/blob-remote/blob_remote_test.go",
			symbol:    "TestExampleBlobRemote",
		},
		"example-rclonestore-artifacts-contract-test": {
			proofType: "test",
			path:      "examples/contract_test.go",
			symbol:    "TestDocsExamplesArtifacts",
		},
		"example-rclonestore-new-source": {
			proofType: "source",
			path:      "rclonestore.go",
			symbol:    "New",
		},
		"example-rclonestore-blob-lifecycle-source": {
			proofType: "source",
			path:      "blobs.go",
			symbol:    "blobStore.Put",
		},
	}
	proofs := make(map[string]bool, len(manifest.ProofSources))
	allowedTypes := map[string]bool{"executable-fixture": true, "test": true, "source": true}
	for _, proof := range manifest.ProofSources {
		expected, ok := expectedProofs[proof.ID]
		if !ok {
			t.Errorf("unexpected proof source ID %q", proof.ID)
		} else if proof.Type != expected.proofType || proof.Path != expected.path || proof.Symbol != expected.symbol {
			t.Errorf("proof source %q = type %q path %q symbol %q, want type %q path %q symbol %q", proof.ID, proof.Type, proof.Path, proof.Symbol, expected.proofType, expected.path, expected.symbol)
		}
		if !allowedTypes[proof.Type] {
			t.Errorf("proof source %q has non-canonical type %q", proof.ID, proof.Type)
		}
		if proofs[proof.ID] {
			t.Errorf("duplicate proof source ID %q", proof.ID)
		}
		proofs[proof.ID] = true
		if strings.Contains(proof.Path, "#") {
			t.Errorf("proof source %q path contains a symbol fragment: %q", proof.ID, proof.Path)
		}
		if _, err := os.Stat(filepath.Join(repositoryRoot, proof.Path)); err != nil {
			t.Errorf("proof source %q does not resolve locally: %v", proof.ID, err)
		}
	}
	if len(manifest.ProofSources) != len(expectedProofs) {
		t.Errorf("proof source count = %d, want %d", len(manifest.ProofSources), len(expectedProofs))
	}

	if len(manifest.Examples) != 1 {
		t.Fatalf("manifest examples = %d, want 1", len(manifest.Examples))
	}
	example := manifest.Examples[0]
	if example.ID != "example-rclonestore-blob-remote-lifecycle" {
		t.Errorf("example ID = %q", example.ID)
	}
	if example.Ecosystem != "go" || example.Owner != "rclonestore" || example.Availability != "source-workspace" {
		t.Errorf("example classification is incorrect: %#v", example)
	}
	if !reflect.DeepEqual(example.Versions, map[string]string{"github.com/looprig/rclonestore": "source-workspace"}) {
		t.Errorf("example versions = %#v", example.Versions)
	}
	if example.SourcePath != "examples/blob-remote/blob_remote_test.go" {
		t.Errorf("example sourcePath = %q", example.SourcePath)
	}
	if example.OfflineCommand != offlineExamplesCommand {
		t.Errorf("example offlineCommand = %q", example.OfflineCommand)
	}
	const assertion = "A deterministic scripted rclone subprocess proves public option forwarding, immutable and idempotent blob puts, typed conflicts and missing-object errors, get/list/delete behavior, reader ownership, and idempotent Store.Close without a real remote."
	if example.Assertion != assertion {
		t.Errorf("example assertion = %q", example.Assertion)
	}
	if example.WorkflowPath != ".github/workflows/docs-examples.yml" || example.JobID != "docs-examples" {
		t.Errorf("example workflow metadata = %q / %q", example.WorkflowPath, example.JobID)
	}
	const cleanup = "The test closes every returned blob reader, calls Store.Close twice, and lets t.TempDir remove the fake executable, config, logs, state, and declared persistence path after each synchronous rclone subprocess exits."
	if example.Cleanup != cleanup {
		t.Errorf("example cleanup = %q", example.Cleanup)
	}
	if string(example.LiveGate) != "null" {
		t.Errorf("example liveGate = %s, want null because the mandatory example never contacts a remote service", example.LiveGate)
	}
	wantProofIDs := []string{
		"example-rclonestore-blob-remote-fixture",
		"example-rclonestore-artifacts-contract-test",
		"example-rclonestore-new-source",
		"example-rclonestore-blob-lifecycle-source",
	}
	if !reflect.DeepEqual(example.ProofIDs, wantProofIDs) {
		t.Errorf("example proofIds = %v, want %v", example.ProofIDs, wantProofIDs)
	}
	for _, proofID := range example.ProofIDs {
		if !proofs[proofID] {
			t.Errorf("example references unknown proof %q", proofID)
		}
	}

	workflow, err := os.ReadFile(filepath.Join(repositoryRoot, ".github/workflows/docs-examples.yml"))
	if err != nil {
		t.Fatalf("read docs examples workflow: %v", err)
	}
	for _, literal := range []string{
		"docs-examples:",
		offlineExamplesCommand,
		"GOWORK=off make check",
	} {
		if !strings.Contains(string(workflow), literal) {
			t.Errorf("workflow does not contain %q", literal)
		}
	}
}
