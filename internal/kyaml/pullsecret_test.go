package kyaml

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestGenerateDockerRegistrySecret(t *testing.T) {
	out, err := Generate(GenParams{
		Kind: "Secret", Name: "sf-registry",
		Registry: "docker-registry.example.net", RegistryUser: "ci",
		RegistryPassword: "s3cr3t", RegistryEmail: "ci@example.net",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var doc struct {
		Type       string            `json:"type"`
		StringData map[string]string `json:"stringData"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("generated secret does not parse: %v\n%s", err, out)
	}
	if doc.Type != "kubernetes.io/dockerconfigjson" {
		t.Errorf("type = %q; an Opaque secret is ignored by the kubelet as a pull secret", doc.Type)
	}
	body, ok := doc.StringData[".dockerconfigjson"]
	if !ok {
		t.Fatalf("no .dockerconfigjson key: %+v", doc.StringData)
	}
	var cfg struct {
		Auths map[string]struct {
			Username, Password, Auth, Email string
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("dockerconfigjson does not parse: %v\n%s", err, body)
	}
	entry, ok := cfg.Auths["docker-registry.example.net"]
	if !ok {
		t.Fatalf("registry missing from auths: %+v", cfg.Auths)
	}
	// The auth field is what every registry client actually sends.
	want := base64.StdEncoding.EncodeToString([]byte("ci:s3cr3t"))
	if entry.Auth != want {
		t.Errorf("auth = %q, want base64 of user:password", entry.Auth)
	}
	if entry.Username != "ci" || entry.Password != "s3cr3t" || entry.Email != "ci@example.net" {
		t.Errorf("entry = %+v", entry)
	}
}

// The round-trip guarantee: everything gen emits must pass verify.
func TestGeneratedPullSecretVerifies(t *testing.T) {
	out, err := Generate(GenParams{
		Kind: "Secret", Name: "sf-registry",
		Registry: "reg.example.net", RegistryUser: "ci", RegistryPassword: "p",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res := Verify([]byte(out)); !res.OK {
		t.Errorf("generated pull secret fails verify: %+v", res.Issues)
	}
}

func TestPullSecretNeedsCredentials(t *testing.T) {
	_, err := Generate(GenParams{Kind: "Secret", Name: "s", Registry: "reg.example.net"})
	if err == nil {
		t.Fatal("a pull secret with no username or password authenticates as nobody; expected an error")
	}
	if !strings.Contains(err.Error(), "username") {
		t.Errorf("error should say what is missing: %v", err)
	}
}

// Without the registry flags a Secret is still the plain Opaque one.
func TestSecretWithoutRegistryIsUnchanged(t *testing.T) {
	out, err := Generate(GenParams{Kind: "Secret", Name: "plain"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "type: Opaque") {
		t.Errorf("plain secret changed shape:\n%s", out)
	}
}

// imagePullSecrets has to land in the pod spec of every kind that has one, or
// the workloads that need the credential are exactly the ones without it.
func TestImagePullSecretReachesEveryPodSpec(t *testing.T) {
	for _, kind := range []string{"Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"} {
		out, err := Generate(GenParams{Kind: kind, Name: "web", ImagePullSecret: "sf-registry"})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !strings.Contains(out, "imagePullSecrets") || !strings.Contains(out, "name: sf-registry") {
			t.Errorf("%s has no imagePullSecrets:\n%s", kind, out)
		}
		if res := Verify([]byte(out)); !res.OK {
			t.Errorf("%s with a pull secret fails verify: %+v", kind, res.Issues)
		}
	}
}

func TestNoImagePullSecretsFieldWhenNotAsked(t *testing.T) {
	out, err := Generate(GenParams{Kind: "Deployment", Name: "web"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.Contains(out, "imagePullSecrets") {
		t.Errorf("an empty imagePullSecrets list should not be emitted:\n%s", out)
	}
}
