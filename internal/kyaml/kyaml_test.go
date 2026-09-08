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

// TestVerifyCatchesKindShapeErrors pins the API-server rules that a manifest
// can violate while still being structurally fine. Each of these was emitted
// by the generator at some point and accepted by the old structural-only
// Verify, only to be rejected by kubectl.
func TestVerifyCatchesKindShapeErrors(t *testing.T) {
	cases := []struct {
		name      string
		manifest  string
		wantField string
	}{
		{
			name: "job with no spec at all",
			manifest: `apiVersion: batch/v1
kind: Job
metadata:
  name: myjob
  selector:
    matchLabels:
      app: myjob
`,
			wantField: "spec",
		},
		{
			name: "job pod template defaults to restartPolicy Always",
			manifest: `apiVersion: batch/v1
kind: Job
metadata:
  name: myjob
spec:
  template:
    spec:
      containers:
        - name: c
          image: busybox:latest
`,
			wantField: "spec.template.spec.restartPolicy",
		},
		{
			name: "job with a hand-written selector",
			manifest: `apiVersion: batch/v1
kind: Job
metadata:
  name: myjob
spec:
  selector:
    matchLabels:
      app: myjob
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: c
          image: busybox:latest
`,
			wantField: "spec.selector",
		},
		{
			name: "daemonset with replicas",
			manifest: `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: myds
spec:
  replicas: 1
  selector:
    matchLabels:
      app: myds
  template:
    spec:
      containers:
        - name: c
          image: nginx:latest
`,
			wantField: "spec.replicas",
		},
		{
			name: "cronjob job template inherits restartPolicy Always",
			manifest: `apiVersion: batch/v1
kind: CronJob
metadata:
  name: mycj
spec:
  schedule: "*/5 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: c
              image: busybox:latest
`,
			wantField: "spec.jobTemplate.spec.template.spec.restartPolicy",
		},
		{
			name: "deployment without selector",
			manifest: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: c
          image: nginx:latest
`,
			wantField: "spec.selector",
		},
		{
			name: "container missing image",
			manifest: `apiVersion: v1
kind: Pod
metadata:
  name: p
spec:
  containers:
    - name: c
`,
			wantField: "spec.containers[0].image",
		},
		{
			name: "statefulset without serviceName",
			manifest: `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
spec:
  replicas: 1
  selector:
    matchLabels:
      app: db
  template:
    spec:
      containers:
        - name: c
          image: postgres:16
`,
			wantField: "spec.serviceName",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Verify([]byte(tc.manifest))
			if res.OK {
				t.Fatalf("expected verification failure for %s", tc.name)
			}
			found := false
			for _, iss := range res.Issues {
				if iss.Field == tc.wantField {
					found = true
				}
			}
			if !found {
				t.Errorf("no issue for field %q; got %+v", tc.wantField, res.Issues)
			}
		})
	}
}

// TestVerifyIgnoresUnknownKinds: CRDs and anything outside KnownKinds get
// structural checks only — we must not invent schema rules for them.
func TestVerifyIgnoresUnknownKinds(t *testing.T) {
	res := Verify([]byte(`apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
spec:
  routes:
    - match: Host(` + "`example.com`" + `)
`))
	if !res.OK {
		t.Errorf("unknown kind should pass structural checks, got %+v", res.Issues)
	}
}

// TestVerifyIssuesCarryLineNumbers: multi-doc issues must point at the line the
// offending document starts on, not line 0.
func TestVerifyIssuesCarryLineNumbers(t *testing.T) {
	manifest := `apiVersion: v1
kind: Namespace
metadata:
  name: ok
---
apiVersion: v1
kind: Service
metadata:
  name: broken
`
	res := Verify([]byte(manifest))
	if res.OK {
		t.Fatal("expected failure: Service without spec.ports")
	}
	for _, iss := range res.Issues {
		if iss.Line != 6 {
			t.Errorf("issue %+v: line = %d, want 6 (start of second document)", iss, iss.Line)
		}
	}
}

// TestGenerateRoundTrip: everything the generator emits must pass Verify.
func TestGenerateRoundTrip(t *testing.T) {
	kindsWithMeta := []string{
		"Namespace", "ConfigMap", "Secret", "Service", "Pod", "PersistentVolumeClaim",
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

// TestEveryKnownKindIsGeneratable: KnownKinds is what `gen` advertises as
// supported, so every entry must actually generate.
func TestEveryKnownKindIsGeneratable(t *testing.T) {
	for kind := range KnownKinds {
		t.Run(kind, func(t *testing.T) {
			out, err := Generate(GenParams{Kind: kind, Name: "gen-test"})
			if err != nil {
				t.Fatalf("advertised as supported but did not generate: %v", err)
			}
			if res := Verify([]byte(out)); !res.OK {
				t.Errorf("generated %s does not verify: %+v\n---\n%s", kind, res.Issues, out)
			}
		})
	}
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

// TestGenerateAcceptsKubectlAliases: someone who types `kubectl get pvc`
// every day will type `gen pvc`. The full name alone is not enough.
func TestGenerateAcceptsKubectlAliases(t *testing.T) {
	cases := map[string]string{
		"pvc": "PersistentVolumeClaim", "PVC": "PersistentVolumeClaim",
		"deploy": "Deployment", "deployment": "Deployment", "Deployment": "Deployment",
		"svc": "Service", "ns": "Namespace", "cm": "ConfigMap",
		"sts": "StatefulSet", "ds": "DaemonSet", "ing": "Ingress",
		"cj": "CronJob", "po": "Pod",
	}
	for alias, want := range cases {
		t.Run(alias, func(t *testing.T) {
			out, err := Generate(GenParams{Kind: alias, Name: "alias-test"})
			if err != nil {
				t.Fatalf("gen %s: %v", alias, err)
			}
			if !strings.Contains(out, "kind: "+want+"\n") {
				t.Errorf("gen %s produced:\n%s\nwant kind: %s", alias, out, want)
			}
			if res := Verify([]byte(out)); !res.OK {
				t.Errorf("gen %s does not verify: %+v", alias, res.Issues)
			}
		})
	}
}

func TestCanonicalKindLeavesUnknownAlone(t *testing.T) {
	if got := canonicalKind("RocketShip"); got != "RocketShip" {
		t.Errorf("canonicalKind = %q, want the input back so the error names it", got)
	}
	if _, err := Generate(GenParams{Kind: "RocketShip", Name: "x"}); err == nil {
		t.Error("an unknown kind should still error")
	}
}

// Services that legitimately have no ports must verify. spec.ports is optional
// in the API; requiring it blocked correct manifests in CI.
func TestVerifyAllowsServicesWithoutPorts(t *testing.T) {
	valid := map[string]string{
		"ExternalName": `apiVersion: v1
kind: Service
metadata:
  name: db
spec:
  type: ExternalName
  externalName: db.example.com
`,
		"headless peer DNS": `apiVersion: v1
kind: Service
metadata:
  name: peers
spec:
  clusterIP: None
  selector:
    app: db
`,
	}
	for name, manifest := range valid {
		t.Run(name, func(t *testing.T) {
			if res := Verify([]byte(manifest)); !res.OK {
				t.Errorf("valid Service rejected: %+v", res.Issues)
			}
		})
	}

	// A normal ClusterIP Service still needs ports.
	res := Verify([]byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: web\nspec:\n  selector:\n    app: web\n"))
	if res.OK {
		t.Error("a ClusterIP Service with no ports should still be rejected")
	}
	// And an ExternalName Service without the name it points at is wrong.
	res = Verify([]byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: db\nspec:\n  type: ExternalName\n"))
	if res.OK {
		t.Error("ExternalName without spec.externalName should be rejected")
	}
}

// A --- inside a block scalar is content, not a document separator. Splitting
// on it reported bogus errors on a manifest kubectl accepts.
func TestVerifyDoesNotSplitInsideBlockScalars(t *testing.T) {
	manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: certs
data:
  bundle.pem: |
    -----BEGIN CERTIFICATE-----
    MIIB
    -----END CERTIFICATE-----
  notes: |
    ---
    not a document separator
`
	res := Verify([]byte(manifest))
	if !res.OK {
		t.Fatalf("block scalar content was treated as a document separator: %+v", res.Issues)
	}
	if len(res.Documents) != 1 {
		t.Errorf("documents = %d, want 1", len(res.Documents))
	}
}

// Real separators, including ones carrying a trailing comment, still split.
func TestVerifySplitsOnRealSeparators(t *testing.T) {
	manifest := `apiVersion: v1
kind: Namespace
metadata:
  name: a
---  # second document
apiVersion: v1
kind: Namespace
metadata:
  name: b
`
	res := Verify([]byte(manifest))
	if !res.OK {
		t.Fatalf("unexpected issues: %+v", res.Issues)
	}
	if len(res.Documents) != 2 {
		t.Fatalf("documents = %d, want 2", len(res.Documents))
	}
	if res.Documents[1].Name != "b" {
		t.Errorf("second document = %q, want b", res.Documents[1].Name)
	}
}

// A manifest authored on Windows separates documents with "---\r". Failing to
// recognise that merged every document into the first, so `verify` reported OK
// having validated only document 1.
func TestVerifyHandlesCRLFSeparators(t *testing.T) {
	crlf := "apiVersion: v1\r\nkind: Namespace\r\nmetadata:\r\n  name: a\r\n" +
		"---\r\n" +
		"apiVersion: v1\r\nkind: Namespace\r\nmetadata:\r\n  name: b\r\n"
	res := Verify([]byte(crlf))
	if !res.OK {
		t.Fatalf("unexpected issues: %+v", res.Issues)
	}
	if len(res.Documents) != 2 {
		t.Fatalf("documents = %d, want 2 — CRLF separators were not recognised", len(res.Documents))
	}
	if res.Documents[1].Name != "b" {
		t.Errorf("second document = %q, want b", res.Documents[1].Name)
	}
}

// And a fault in a later CRLF document must actually be reported.
func TestVerifyCatchesErrorsInLaterCRLFDocuments(t *testing.T) {
	crlf := "apiVersion: v1\r\nkind: Namespace\r\nmetadata:\r\n  name: ok\r\n" +
		"---\r\n" +
		"apiVersion: apps/v1\r\nkind: DaemonSet\r\nmetadata:\r\n  name: bad\r\n" +
		"spec:\r\n  replicas: 1\r\n  selector:\r\n    matchLabels:\r\n      app: x\r\n" +
		"  template:\r\n    spec:\r\n      containers:\r\n        - name: c\r\n          image: nginx\r\n"
	res := Verify([]byte(crlf))
	if res.OK {
		t.Fatal("a DaemonSet with spec.replicas in document 2 was not reported")
	}
	found := false
	for _, iss := range res.Issues {
		if iss.Field == "spec.replicas" {
			found = true
		}
	}
	if !found {
		t.Errorf("issues = %+v, want the document-2 spec.replicas problem", res.Issues)
	}
}

// A file with no trailing newline, and one that is only a separator.
func TestVerifyEdgeCaseDocumentStreams(t *testing.T) {
	if res := Verify([]byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a")); !res.OK {
		t.Errorf("no trailing newline should still verify: %+v", res.Issues)
	}
	// A leading separator is legal YAML and introduces the first document.
	res := Verify([]byte("---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n"))
	if !res.OK || len(res.Documents) != 1 {
		t.Errorf("leading separator: OK=%v docs=%d issues=%+v", res.OK, len(res.Documents), res.Issues)
	}
	if res.Documents[0].Name != "a" {
		t.Errorf("name = %q, want a", res.Documents[0].Name)
	}
	// A stream that is only separators has no documents at all.
	if res := Verify([]byte("---\n---\n")); res.OK {
		t.Error("a stream of only separators should not verify")
	}
}
