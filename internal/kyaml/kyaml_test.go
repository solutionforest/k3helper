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
