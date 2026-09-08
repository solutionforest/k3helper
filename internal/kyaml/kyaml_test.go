package kyaml

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtures = "../../test/fixtures/yaml"

func TestVerifyGoodDocs(t *testing.T) {
	entries, _ := os.ReadDir(filepath.Join(fixtures, "good"))
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(fixtures, "good", e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			res := Verify(data)
			if !res.OK {
				t.Fatalf("expected OK, got issues: %+v", res.Issues)
			}
			if len(res.Documents) == 0 {
				t.Error("expected at least one document")
			}
		})
	}
}

func TestVerifyMultiDoc(t *testing.T) {
	data, _ := os.ReadFile(filepath.Join(fixtures, "good", "multi-doc.yaml"))
	res := Verify(data)
	if !res.OK {
		t.Fatalf("expected OK, got issues: %+v", res.Issues)
	}
	if len(res.Documents) != 2 {
		t.Errorf("documents = %d, want 2", len(res.Documents))
	}
	if res.Documents[0].Kind != "Service" || res.Documents[1].Kind != "ConfigMap" {
		t.Errorf("kinds = %s, %s; want Service, ConfigMap", res.Documents[0].Kind, res.Documents[1].Kind)
	}
}

func TestVerifyBadSyntax(t *testing.T) {
	entries, _ := os.ReadDir(filepath.Join(fixtures, "bad-syntax"))
	if len(entries) == 0 {
		t.Fatal("no bad-syntax fixtures")
	}
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			data, _ := os.ReadFile(filepath.Join(fixtures, "bad-syntax", e.Name()))
			res := Verify(data)
			if res.OK {
				t.Error("expected failure for bad syntax")
			}
		})
	}
}

func TestVerifyBadStructure(t *testing.T) {
	entries, _ := os.ReadDir(filepath.Join(fixtures, "bad-structure"))
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			data, _ := os.ReadFile(filepath.Join(fixtures, "bad-structure", e.Name()))
			res := Verify(data)
			if res.OK {
				t.Error("expected structural failure")
			}
			if len(res.Issues) == 0 {
				t.Error("expected issues to be reported")
			}
		})
	}
}

func TestVerifyEmptyInput(t *testing.T) {
	res := Verify([]byte(""))
	if res.OK {
		t.Error("expected failure for empty input")
	}
	res = Verify([]byte("# just a comment\n"))
	if res.OK {
		t.Error("expected failure for comment-only input")
	}
}

// TestGenerateRoundTrip: everything the generator emits must pass Verify.
func TestGenerateRoundTrip(t *testing.T) {
	kindsWithMeta := []string{
		"Namespace", "ConfigMap", "Secret", "Service", "PersistentVolumeClaim",
		"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Ingress",
	}
	for _, kind := range kindsWithMeta {
		for _, ns := range []string{"", "prod"} {
			t.Run(kind+"-ns-"+boolStr(ns != ""), func(t *testing.T) {
				out, err := Generate(GenParams{Kind: kind, Name: "rt-test", Namespace: ns, Image: "nginx:1.25", Port: 8080})
				if err != nil {
					t.Fatalf("generate: %v", err)
				}
				res := Verify([]byte(out))
				if !res.OK {
					t.Errorf("generated %s does not verify: %+v\n---\n%s", kind, res.Issues, out)
				}
			})
		}
	}
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func TestGenerateErrors(t *testing.T) {
	if _, err := Generate(GenParams{Kind: "Deployment", Name: ""}); err == nil {
		t.Error("expected error for empty name")
	}
	if _, err := Generate(GenParams{Kind: "", Name: "x"}); err == nil {
		t.Error("expected error for empty kind")
	}
	if _, err := Generate(GenParams{Kind: "RocketShip", Name: "x"}); err == nil {
		t.Error("expected error for unsupported kind")
	}
}

func TestGenerateCorrectAPIVersions(t *testing.T) {
	cases := map[string]string{
		"Deployment": "apps/v1",
		"Job":        "batch/v1",
		"CronJob":    "batch/v1",
		"Ingress":    "networking.k8s.io/v1",
		"Service":    "v1",
	}
	for kind, wantAPI := range cases {
		out, err := Generate(GenParams{Kind: kind, Name: "v-test"})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !strings.Contains(out, "apiVersion: "+wantAPI+"\n") {
			t.Errorf("%s: output missing apiVersion %s:\n%s", kind, wantAPI, out)
		}
	}
}
