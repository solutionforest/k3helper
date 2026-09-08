//go:build integration

package deploy

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/k3helper/internal/ssh"
)

// TestDeployLiveDeployDemo applies a tiny nginx deployment on the sandbox
// server and waits for rollout.
func TestDeployLiveDeployDemo(t *testing.T) {
	if os.Getenv("K3HELPER_SANDBOX") != "1" {
		t.Skip("set K3HELPER_SANDBOX=1 with live sandbox")
	}
	server, err := ssh.Dial(ssh.Node{Host: "192.168.139.177", Port: 22, User: "sandbox", Key: "../../test/sandbox/ssh/id_ed25519"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	dir := t.TempDir()
	manifest := filepath.Join(dir, "demo.yaml")
	if err := os.WriteFile(manifest, []byte(demoManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scpTo(server, manifest, "/tmp/k3helper-demo.yaml"); err != nil {
		t.Fatalf("copy manifest: %v", err)
	}

	res, err := Deploy(server, "/tmp/k3helper-demo.yaml", Options{WaitTimeout: 90 * time.Second})
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
	server.Run("sudo -n k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml delete deployment k3helper-demo --ignore-not-found")
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

// scpTo copies a local file to the remote host via base64 over ssh.
func scpTo(c *ssh.Client, localPath, remotePath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	enc := base64.StdEncoding.EncodeToString(data)
	_, code, err := c.Run("echo " + enc + " | base64 -d > " + remotePath + " && chmod 644 " + remotePath)
	if err != nil || code != 0 {
		return err
	}
	return nil
}
