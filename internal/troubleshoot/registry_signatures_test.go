package troubleshoot

import (
	"strings"
	"testing"
)

// evidenceWithPullEvent builds a bundle whose only fault is one image-pull
// failure carrying the given message.
func evidenceWithPullEvent(msg string) Evidence {
	return Evidence{
		PodEvents:   map[string][]string{"default/web-1": {"Failed to pull image \"reg/app:1\": " + msg}},
		PodStatuses: map[string]string{"default/web-1": "ImagePullBackOff"},
	}
}

func find(ds []Diagnosis, id string) (Diagnosis, bool) {
	for _, d := range ds {
		if d.SignatureID == id {
			return d, true
		}
	}
	return Diagnosis{}, false
}

// ImagePullBackOff is one symptom of four different problems. The kubelet
// records which, so the diagnosis should say which.
func TestPullFailuresAreClassifiedByWhatTheRegistrySaid(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"unauthorized", `failed to authorize: 401 Unauthorized`, "registry.auth"},
		{"access denied", `pull access denied, repository does not exist or may require authorization`, "registry.auth"},
		{"unknown CA", `tls: failed to verify certificate: x509: certificate signed by unknown authority`, "registry.cert"},
		// Verbatim from a real cluster whose registries.yaml named a CA that
		// had not been copied to the node. None of the TLS phrases above
		// appear in it, and the first version of this matcher missed it.
		{"ca_file not on the node",
			`failed to resolve reference "reg/app:1.2": unable to read CA cert "/etc/ssl/certs/internal-ca.crt": ` +
				`open /etc/ssl/certs/internal-ca.crt: no such file or directory`, "registry.cert"},
		{"http registry", `http: server gave HTTP response to HTTPS client`, "registry.cert"},
		{"dns", `dial tcp: lookup reg.example.net: no such host`, "registry.unreachable"},
		{"refused", `dial tcp 10.0.0.9:443: connect: connection refused`, "registry.unreachable"},
	}
	for _, c := range cases {
		ds := Diagnose(evidenceWithPullEvent(c.msg))
		got, ok := find(ds, c.want)
		if !ok {
			t.Errorf("%s: %q did not produce %s; got %v", c.name, c.msg, c.want, ids(ds))
			continue
		}
		if got.Confidence < 70 {
			t.Errorf("%s: confidence %d is too low to rank above the generic symptom", c.name, got.Confidence)
		}
	}
}

// The cause must outrank the symptom — the defect this whole ranking exists to
// avoid is sending an operator to check credentials when DNS was the problem.
func TestSpecificRegistryCauseOutranksGenericImagePull(t *testing.T) {
	ds := Diagnose(evidenceWithPullEvent(`failed to authorize: 401 Unauthorized`))
	if len(ds) == 0 {
		t.Fatal("no diagnosis at all")
	}
	if ds[0].SignatureID != "registry.auth" {
		t.Errorf("top finding = %s, want registry.auth; full ranking: %v", ds[0].SignatureID, ids(ds))
	}
	generic, ok := find(ds, "pod.imagepull")
	if !ok {
		t.Fatal("the generic finding should still be reported, just below the cause")
	}
	auth, _ := find(ds, "registry.auth")
	if generic.Confidence >= auth.Confidence {
		t.Errorf("symptom (%d) must not outrank cause (%d)", generic.Confidence, auth.Confidence)
	}
}

// An auth failure behind an untrusted certificate is a certificate problem:
// fixing the credential would not help.
func TestCertificateBeatsAuthWhenBothWordsAppear(t *testing.T) {
	f := classifyPullFailures(evidenceWithPullEvent(
		`x509: certificate signed by unknown authority: 401 Unauthorized`))
	if f.cert != 1 || f.auth != 0 {
		t.Errorf("classification = %+v; want the certificate cause", f)
	}
}

// A pull failure with no recognisable cause stays the generic finding at full
// confidence — the wrong tag is still the commonest reason of all.
func TestUnrecognisedPullFailureKeepsTheGenericFinding(t *testing.T) {
	ds := Diagnose(evidenceWithPullEvent(`manifest unknown`))
	got, ok := find(ds, "pod.imagepull")
	if !ok {
		t.Fatal("generic image-pull finding missing")
	}
	if got.Confidence < 80 {
		t.Errorf("confidence = %d; nothing more specific matched, so it should not be demoted", got.Confidence)
	}
	for _, id := range []string{"registry.auth", "registry.cert", "registry.unreachable"} {
		if _, ok := find(ds, id); ok {
			t.Errorf("%s fired on a message that says nothing about the registry", id)
		}
	}
}

// A healthy cluster must produce none of these.
func TestNoRegistryFindingsOnHealthyEvidence(t *testing.T) {
	ds := Diagnose(Evidence{PodStatuses: map[string]string{"default/web-1": "Running"}})
	for _, id := range []string{"registry.auth", "registry.cert", "registry.unreachable", "pod.imagepull"} {
		if _, ok := find(ds, id); ok {
			t.Errorf("%s fired on a healthy cluster", id)
		}
	}
}

// The remediation is the reason a finding exists; each must name the concrete
// command that fixes it.
func TestRegistryRemediationsAreActionable(t *testing.T) {
	cases := map[string]string{
		"registry.auth":        "registry apply",
		"registry.cert":        "ca_file",
		"registry.unreachable": "getent hosts",
	}
	for id, want := range cases {
		var found bool
		for _, sig := range registry {
			if sig.ID == id {
				found = true
				if !strings.Contains(sig.Remediation, want) {
					t.Errorf("%s remediation does not mention %q: %s", id, want, sig.Remediation)
				}
			}
		}
		if !found {
			t.Errorf("signature %s is not registered", id)
		}
	}
}

func ids(ds []Diagnosis) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.SignatureID)
	}
	return out
}
