package check

import (
	"strings"
	"testing"
)

const goodRegistriesYAML = `mirrors:
  reg.example.net:
    endpoint:
      - https://reg.example.net
configs:
  reg.example.net:
    auth:
      username: ci
      password: s3cr3t
    tls:
      ca_file: /etc/ssl/certs/internal-ca.crt
`

func registryCtx(file string, extra map[string]struct {
	Out  string
	Code int
}) Context {
	m := MapExec{
		`sudo -n cat /etc/rancher/k3s/registries.yaml 2>/dev/null`: {Out: file, Code: 0},
	}
	for k, v := range extra {
		m[k] = v
	}
	return Context{Exec: m}
}

func TestRegistryCheckHealthy(t *testing.T) {
	ctx := registryCtx(goodRegistriesYAML, map[string]struct {
		Out  string
		Code int
	}{
		`test -r '/etc/ssl/certs/internal-ca.crt'`: {Out: "", Code: 0},
	})
	res := RegistryCheck{}.Run(ctx)
	if res.Status != OK {
		t.Fatalf("status = %s (%s)", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "reg.example.net") {
		t.Errorf("summary should name the registry: %s", res.Summary)
	}
}

// A ca_file that is not on the node fails every pull from that registry, and
// surfaces only as ImagePullBackOff — which is why it is worth checking here.
func TestRegistryCheckMissingCAFile(t *testing.T) {
	ctx := registryCtx(goodRegistriesYAML, map[string]struct {
		Out  string
		Code int
	}{
		`test -r '/etc/ssl/certs/internal-ca.crt'`: {Out: "", Code: 1},
	})
	res := RegistryCheck{}.Run(ctx)
	if res.Status != Fail {
		t.Fatalf("status = %s, want FAIL (%s)", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "internal-ca.crt") {
		t.Errorf("summary should name the missing file: %s", res.Summary)
	}
}

// k3s ignores the whole file when it cannot parse it, so every private
// registry on the node quietly falls back to an anonymous public pull.
func TestRegistryCheckUnparseableFile(t *testing.T) {
	res := RegistryCheck{}.Run(registryCtx("mirrors: [this is: not valid\n", nil))
	if res.Status != Fail {
		t.Fatalf("status = %s, want FAIL", res.Status)
	}
	if !strings.Contains(res.Remediation, "registry apply") {
		t.Errorf("remediation should offer the rewrite: %s", res.Remediation)
	}
}

func TestRegistryCheckInsecureIsAWarning(t *testing.T) {
	insecure := `mirrors:
  reg.example.net:
    endpoint:
      - https://reg.example.net
configs:
  reg.example.net:
    tls:
      insecure_skip_verify: true
`
	res := RegistryCheck{}.Run(registryCtx(insecure, nil))
	if res.Status != Warn {
		t.Fatalf("status = %s, want WARN (%s)", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "reg.example.net") {
		t.Errorf("summary should name which registry: %s", res.Summary)
	}
}

// No file is the normal case for a cluster pulling from public registries.
// Skip, not OK: nothing was actually verified.
func TestRegistryCheckNoFileSkips(t *testing.T) {
	res := RegistryCheck{}.Run(Context{Exec: MapExec{}})
	if res.Status != Skip {
		t.Errorf("status = %s, want SKIP (%s)", res.Status, res.Summary)
	}
}

// A configs block with no matching mirror is not applied by k3s to a registry
// it does not otherwise know about — the file looks configured and is not.
func TestRegistryCheckConfigWithoutMirror(t *testing.T) {
	orphan := `configs:
  reg.example.net:
    auth:
      username: ci
      password: p
`
	res := RegistryCheck{}.Run(registryCtx(orphan, nil))
	if res.Status != Warn {
		t.Errorf("status = %s, want WARN (%s)", res.Status, res.Summary)
	}
}
