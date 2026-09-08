//go:build integration

package deploy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/k3helper/internal/deploy"
	"github.com/solutionforest/k3helper/internal/kyaml"
	"github.com/solutionforest/k3helper/internal/sandbox"
)

// TestDeployLiveDeployDemo deploys a local manifest to the sandbox cluster.
// The manifest is never copied by hand: shipping it to the target is Deploy's
// job, and this test exists to prove it.
func TestDeployLiveDeployDemo(t *testing.T) {
	server := sandbox.Dial(t, "server")

	manifest := filepath.Join(t.TempDir(), "demo.yaml")
	if err := os.WriteFile(manifest, []byte(demoManifest), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := deploy.Deploy(server, manifest, deploy.Options{WaitTimeout: 90 * time.Second})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(res.RolledOut) != 1 || !strings.HasPrefix(res.RolledOut[0], "deployment.apps/k3helper-demo") {
		t.Errorf("rolledOut = %v", res.RolledOut)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %v", res.Errors)
	}

	// cleanup
	server.Run(kyaml.KubectlBase + " delete deployment k3helper-demo --ignore-not-found")
}

// TestGeneratedManifestsPassServerDryRun is the round-trip guarantee with
// teeth: every kind `gen` supports must be accepted by a real API server.
// The offline rules in Verify cannot prove this on their own.
func TestGeneratedManifestsPassServerDryRun(t *testing.T) {
	server := sandbox.Dial(t, "server")

	for kind := range kyaml.KnownKinds {
		t.Run(kind, func(t *testing.T) {
			out, err := kyaml.Generate(kyaml.GenParams{
				Kind:  kind,
				Name:  "gen-" + strings.ToLower(kind),
				Image: "busybox:latest",
			})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			res, err := kyaml.VerifyLive(server, []byte(out), kind+".yaml")
			if err != nil {
				t.Fatalf("verify against server: %v", err)
			}
			if !res.OK {
				t.Errorf("API server rejected generated %s: %+v\n---\n%s", kind, res.Issues, out)
			}
		})
	}
}

const demoManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: k3helper-demo
spec:
  replicas: 1
  selector:
    matchLabels:
      app: k3helper-demo
  template:
    metadata:
      labels:
        app: k3helper-demo
    spec:
      containers:
      - name: web
        image: nginx:alpine
        ports:
        - containerPort: 80
`
