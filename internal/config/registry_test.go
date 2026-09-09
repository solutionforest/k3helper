package config

import (
	"strings"
	"testing"
)

func targetsWith(regs ...Registry) *Targets {
	return &Targets{
		Cluster:    "test",
		Nodes:      []Node{{Name: "s1", Role: "server", Host: "10.0.0.1", User: "u"}},
		Registries: regs,
	}
}

func TestRegistryURLDefaultsToHTTPS(t *testing.T) {
	if got := (Registry{Host: "reg.example.net"}).URL(); got != "https://reg.example.net" {
		t.Errorf("URL = %q", got)
	}
	if got := (Registry{Host: "docker.io", Endpoint: "http://mirror:5000"}).URL(); got != "http://mirror:5000" {
		t.Errorf("URL = %q", got)
	}
}

// A targets file goes into a repository; the password should not have to.
func TestPasswordEnv(t *testing.T) {
	t.Setenv("K3HELPER_TEST_REGISTRY_PW", "s3cr3t")
	r, err := Registry{Host: "r", Username: "ci", PasswordEnv: "K3HELPER_TEST_REGISTRY_PW"}.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Password != "s3cr3t" {
		t.Errorf("password = %q", r.Password)
	}

	// An unset variable fails here rather than becoming an anonymous pull that
	// fails later, on another machine, as "unauthorized".
	_, err = Registry{Host: "r", Username: "ci", PasswordEnv: "K3HELPER_TEST_UNSET_PW"}.Resolve()
	if err == nil {
		t.Fatal("an unset password_env must be an error")
	}
	if !strings.Contains(err.Error(), "K3HELPER_TEST_UNSET_PW") {
		t.Errorf("error should name the variable: %v", err)
	}
}

func TestRegistryValidation(t *testing.T) {
	cases := []struct {
		name string
		reg  Registry
		want string // substring of the expected error; "" means valid
	}{
		{"valid anonymous", Registry{Host: "reg.example.net"}, ""},
		{"valid with auth", Registry{Host: "r", Username: "u", Password: "p"}, ""},
		{"valid with password_env", Registry{Host: "r", Username: "u", PasswordEnv: "V"}, ""},
		{"no host", Registry{Username: "u", Password: "p"}, "host is required"},
		{"host is a URL", Registry{Host: "https://reg.example.net"}, "not a URL"},
		{"both passwords", Registry{Host: "r", Username: "u", Password: "p", PasswordEnv: "V"}, "not both"},
		{"password without user", Registry{Host: "r", Password: "p"}, "without a username"},
		{"user without password", Registry{Host: "r", Username: "u"}, "no password"},
		{"ca and insecure", Registry{Host: "r", CAFile: "/ca.crt", InsecureSkipVerify: true}, "contradict"},
		{"endpoint without scheme", Registry{Host: "r", Endpoint: "mirror:5000"}, "needs a scheme"},
	}
	for _, c := range cases {
		err := targetsWith(c.reg).Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", c.name, err)
		case c.want != "" && err == nil:
			t.Errorf("%s: expected an error containing %q", c.name, c.want)
		case c.want != "" && err != nil && !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: error = %v, want it to mention %q", c.name, err, c.want)
		}
	}
}

func TestDuplicateRegistryHostRejected(t *testing.T) {
	err := targetsWith(Registry{Host: "r"}, Registry{Host: "r"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("err = %v; two entries for one host cannot both apply", err)
	}
}

func TestResolvedRegistriesPropagatesFailure(t *testing.T) {
	tg := targetsWith(Registry{Host: "r", Username: "u", PasswordEnv: "K3HELPER_TEST_STILL_UNSET"})
	if _, err := tg.ResolvedRegistries(); err == nil {
		t.Fatal("expected the unset variable to surface before anything is written to a node")
	}
}
