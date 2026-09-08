package troubleshoot

import (
	"strings"
	"testing"
	"time"
)

func topSignature(t *testing.T, e Evidence) Diagnosis {
	t.Helper()
	d := Diagnose(e)
	if len(d) == 0 {
		t.Fatal("expected at least one diagnosis, got none")
	}
	return d[0]
}

func TestEtcdQuorumSignature(t *testing.T) {
	cases := []struct {
		name       string
		ready      int
		desired    int
		wantFires  bool
		wantMinCnf int
	}{
		{"healthy 3/3", 3, 3, false, 0},
		{"degraded but quorate 2/3", 2, 3, true, 60},
		{"quorum lost 1/3", 1, 3, true, 90},
		{"quorum lost 2/5", 2, 5, true, 90},
		{"degraded quorate 4/5", 4, 5, true, 60},
		{"healthy 1/1 single server", 1, 1, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Evidence{Etcd: &ReadyRatio{Ready: tc.ready, Desired: tc.desired}}
			d := Diagnose(e)
			if !tc.wantFires {
				for _, x := range d {
					if x.SignatureID == "cluster.etcd-quorum" {
						t.Fatalf("signature fired on a healthy cluster: %+v", x)
					}
				}
				return
			}
			if len(d) == 0 || d[0].SignatureID != "cluster.etcd-quorum" {
				t.Fatalf("expected cluster.etcd-quorum, got %+v", d)
			}
			if d[0].Confidence < tc.wantMinCnf {
				t.Errorf("confidence = %d, want >= %d", d[0].Confidence, tc.wantMinCnf)
			}
		})
	}
}

// A sqlite-backed k3s has no etcd at all; the signature must stay silent
// rather than reporting "0 of 0 members ready".
func TestEtcdQuorumSilentWithoutEtcd(t *testing.T) {
	for _, e := range []Evidence{
		{Etcd: nil},
		{Etcd: &ReadyRatio{Ready: 0, Desired: 0}},
	} {
		if d := Diagnose(e); len(d) != 0 {
			t.Errorf("expected no diagnosis without etcd, got %+v", d)
		}
	}
}

func TestCertExpirySignature(t *testing.T) {
	cases := []struct {
		name      string
		days      int
		subject   string
		wantFires bool
	}{
		{"fresh, 200 days", 200, "CN=k3s-serving", false},
		{"31 days", 31, "CN=k3s-serving", false},
		{"30 days", 30, "CN=k3s-serving", true},
		{"5 days", 5, "CN=k3s-serving", true},
		{"already expired", -3, "CN=k3s-serving", true},
		{"no evidence gathered", 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Evidence{CertExpiryDays: tc.days, CertSubject: tc.subject}
			d := Diagnose(e)
			fired := len(d) > 0 && d[0].SignatureID == "cluster.cert-expiry"
			if fired != tc.wantFires {
				t.Fatalf("fired = %v, want %v (diagnoses: %+v)", fired, tc.wantFires, d)
			}
			if fired && !strings.Contains(d[0].Remediation, "restart k3s") {
				t.Error("remediation should say how to rotate the certificates")
			}
		})
	}
}

// An expired certificate must outrank one merely expiring soon.
func TestCertExpiryRanksExpiredHigher(t *testing.T) {
	expired := topSignature(t, Evidence{CertExpiryDays: -1, CertSubject: "CN=k3s-serving"})
	soon := topSignature(t, Evidence{CertExpiryDays: 25, CertSubject: "CN=k3s-serving"})
	if expired.Confidence <= soon.Confidence {
		t.Errorf("expired confidence %d should exceed expiring-soon %d", expired.Confidence, soon.Confidence)
	}
}

func TestCoreDNSSignature(t *testing.T) {
	cases := []struct {
		name      string
		ratio     *ReadyRatio
		wantFires bool
		wantConf  int
	}{
		{"healthy 2/2", &ReadyRatio{2, 2}, false, 0},
		{"partial 1/2", &ReadyRatio{1, 2}, true, 55},
		{"down 0/2", &ReadyRatio{0, 2}, true, 95},
		// Scaled to zero: the deployment exists but serves nothing. DNS is
		// down for the whole cluster regardless of whether that was deliberate.
		{"scaled to zero 0/0", &ReadyRatio{0, 0}, true, 95},
		{"not gathered", nil, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Diagnose(Evidence{CoreDNS: tc.ratio})
			fired := len(d) > 0 && d[0].SignatureID == "network.coredns"
			if fired != tc.wantFires {
				t.Fatalf("fired = %v, want %v (%+v)", fired, tc.wantFires, d)
			}
			if fired && d[0].Confidence != tc.wantConf {
				t.Errorf("confidence = %d, want %d", d[0].Confidence, tc.wantConf)
			}
		})
	}
}

func TestEmptyEndpointsSignature(t *testing.T) {
	d := topSignature(t, Evidence{EmptyEndpoints: []string{"prod/web", "prod/api"}})
	if d.SignatureID != "network.empty-endpoints" {
		t.Fatalf("expected network.empty-endpoints, got %+v", d)
	}
	if d.Confidence < 50 {
		t.Errorf("confidence = %d, want >= 50", d.Confidence)
	}
	if len(Diagnose(Evidence{EmptyEndpoints: nil})) != 0 {
		t.Error("no empty endpoints should yield no diagnosis")
	}
}

// A total DNS outage must outrank an individual Service having no backends,
// because CoreDNS being down explains the whole cluster misbehaving.
func TestCoreDNSOutranksEmptyEndpoints(t *testing.T) {
	e := Evidence{
		CoreDNS:        &ReadyRatio{Ready: 0, Desired: 2},
		EmptyEndpoints: []string{"prod/web"},
	}
	d := Diagnose(e)
	if len(d) < 2 {
		t.Fatalf("expected both signatures to fire, got %+v", d)
	}
	if d[0].SignatureID != "network.coredns" {
		t.Errorf("top diagnosis = %s, want network.coredns", d[0].SignatureID)
	}
}

// The healthy-cluster guarantee has to hold as signatures are added: a fully
// healthy evidence bundle must still produce zero findings.
func TestHealthyClusterWithNewEvidenceYieldsNoDiagnoses(t *testing.T) {
	e := Evidence{
		PodEvents:      map[string][]string{},
		PodStatuses:    map[string]string{},
		K3sService:     map[string]string{"server": "active", "agent1": "active"},
		NodeNotReady:   []string{},
		CoreDNS:        &ReadyRatio{Ready: 2, Desired: 2},
		Etcd:           &ReadyRatio{Ready: 3, Desired: 3},
		CertExpiryDays: 300,
		CertSubject:    "CN=k3s-serving",
		EmptyEndpoints: nil,
	}
	if d := Diagnose(e); len(d) != 0 {
		t.Errorf("healthy cluster produced findings: %+v", d)
	}
}

func TestParseReadyRatio(t *testing.T) {
	cases := []struct {
		in            string
		ready, desire int
		ok            bool
	}{
		{"2/2", 2, 2, true},
		{"0/2", 0, 2, true},
		{"/2", 0, 2, true}, // readyReplicas is absent when zero
		{" 1/3 \n", 1, 3, true},
		{"", 0, 0, false},
		{"garbage", 0, 0, false},
		{"a/b", 0, 0, false},
	}
	for _, tc := range cases {
		r, d, ok := parseReadyRatio(tc.in)
		if ok != tc.ok || r != tc.ready || d != tc.desire {
			t.Errorf("parseReadyRatio(%q) = %d,%d,%v; want %d,%d,%v", tc.in, r, d, ok, tc.ready, tc.desire, tc.ok)
		}
	}
}

// TestParseCertExpiryRealK3sOutput pins the exact format `k3s certificate
// check` emits (captured from k3s on the sandbox). It writes to stderr, uses
// RFC3339 timestamps after "expires at", and carries no CN= — so the subject
// falls back to the .crt filename.
func TestParseCertExpiryRealK3sOutput(t *testing.T) {
	out := `time="2026-09-08T18:17:37+08:00" level=info msg="Server detected, checking agent and server certificates"
time="2026-09-08T18:17:37+08:00" level=info msg="client-kube-apiserver.crt: certificate system:apiserver (ClientAuth) is ok, expires at 2027-09-08T08:45:21Z"
time="2026-09-08T18:17:37+08:00" level=info msg="client-kube-apiserver.crt: certificate k3s-client-ca@1788860721 (CertSign) is ok, expires at 2036-09-05T08:45:21Z"
time="2026-09-08T18:17:37+08:00" level=info msg="serving-kube-apiserver.crt: certificate kube-apiserver (ServerAuth) is ok, expires at 2027-09-08T08:45:21Z"
time="2026-09-08T18:17:37+08:00" level=info msg="client-admin.crt: certificate system:admin (ClientAuth) is ok, expires at 2027-09-08T08:45:21Z"`

	// one year out from the sandbox's clock
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	days, subject := parseCertExpiry(out, now)
	if days != 365 {
		t.Errorf("days = %d, want 365 (soonest cert 2027-09-08)", days)
	}
	if subject != "client-kube-apiserver.crt" {
		t.Errorf("subject = %q, want the .crt filename when no CN= is present", subject)
	}

	// A year out must not raise a finding.
	if d := Diagnose(Evidence{CertExpiryDays: days, CertSubject: subject}); len(d) != 0 {
		t.Errorf("healthy certificates produced findings: %+v", d)
	}

	// The same output read shortly before expiry must fire.
	nearly := time.Date(2027, 9, 1, 0, 0, 0, 0, time.UTC)
	days, subject = parseCertExpiry(out, nearly)
	if days != 7 {
		t.Fatalf("days = %d, want 7", days)
	}
	d := Diagnose(Evidence{CertExpiryDays: days, CertSubject: subject})
	if len(d) == 0 || d[0].SignatureID != "cluster.cert-expiry" {
		t.Errorf("expected cert-expiry finding, got %+v", d)
	}
}

func TestParseCertExpiry(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)

	// real `k3s certificate check` shape, several certs: report the soonest
	out := `Checking certificates
Certificate CN=k3s-serving, expires 2027-01-05
Certificate CN=k3s-client-ca, expires 2026-09-20
Certificate CN=etcd-server, expires 2026-12-01`
	days, subject := parseCertExpiry(out, now)
	if days != 12 {
		t.Errorf("days = %d, want 12 (soonest cert)", days)
	}
	if subject != "CN=k3s-client-ca" {
		t.Errorf("subject = %q, want the soonest-expiring cert", subject)
	}

	// already expired yields a negative count
	if days, _ := parseCertExpiry("Certificate CN=x, expires 2026-09-01", now); days != -7 {
		t.Errorf("expired cert days = %d, want -7", days)
	}

	// unparseable output must read as "no evidence", not "expires today"
	for _, bad := range []string{"", "some unrelated output", "expires never"} {
		if days, subject := parseCertExpiry(bad, now); days != 0 || subject != "" {
			t.Errorf("parseCertExpiry(%q) = %d,%q; want 0,\"\"", bad, days, subject)
		}
	}
}

func TestParseEndpoints(t *testing.T) {
	data := `{"items":[
	  {"metadata":{"namespace":"prod","name":"web"},"subsets":[{"addresses":[{"ip":"10.42.0.5"}]}]},
	  {"metadata":{"namespace":"prod","name":"api"},"subsets":[]},
	  {"metadata":{"namespace":"prod","name":"cache"},"subsets":[{"addresses":[]}]},
	  {"metadata":{"namespace":"default","name":"kubernetes"},"subsets":[]}
	]}`
	e := &Evidence{}
	parseEndpoints(data, e)

	want := []string{"prod/api", "prod/cache"}
	if len(e.EmptyEndpoints) != len(want) {
		t.Fatalf("EmptyEndpoints = %v, want %v", e.EmptyEndpoints, want)
	}
	for i := range want {
		if e.EmptyEndpoints[i] != want[i] {
			t.Errorf("EmptyEndpoints[%d] = %q, want %q", i, e.EmptyEndpoints[i], want[i])
		}
	}
}

// The default/kubernetes endpoint is managed by the control plane and never
// has pod-backed addresses; flagging it would fire on every healthy cluster.
func TestParseEndpointsSkipsKubernetesService(t *testing.T) {
	e := &Evidence{}
	parseEndpoints(`{"items":[{"metadata":{"namespace":"default","name":"kubernetes"},"subsets":[]}]}`, e)
	if len(e.EmptyEndpoints) != 0 {
		t.Errorf("default/kubernetes should be ignored, got %v", e.EmptyEndpoints)
	}
}

// --- host-layer signatures ---

func TestContainerRuntimeDownSignature(t *testing.T) {
	cases := []struct {
		name  string
		state map[string]string
		fires bool
	}{
		{"healthy", map[string]string{"agent1": "containerd=active"}, false},
		{"failed", map[string]string{"agent1": "containerd=failed"}, true},
		{"inactive", map[string]string{"agent1": "containerd=inactive"}, true},
		{"docker runtime", map[string]string{"agent1": "docker=failed"}, true},
		// k3s embeds containerd, so no separate unit is normal, not a fault.
		{"no evidence", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Diagnose(Evidence{ContainerRuntime: tc.state})
			got := hasSig(d, "node.runtime-down")
			if got != tc.fires {
				t.Errorf("fired = %v, want %v (%+v)", got, tc.fires, d)
			}
		})
	}
}

func TestClockSkewSignature(t *testing.T) {
	cases := []struct {
		skew  time.Duration
		fires bool
		conf  int
	}{
		{time.Second, false, 0},
		{30 * time.Second, false, 0},
		{90 * time.Second, true, 55},
		{10 * time.Minute, true, 90},
	}
	for _, tc := range cases {
		d := Diagnose(Evidence{ClockSkew: map[string]time.Duration{"n": tc.skew}})
		if got := hasSig(d, "node.clock-skew"); got != tc.fires {
			t.Errorf("skew %v: fired = %v, want %v", tc.skew, got, tc.fires)
			continue
		}
		if tc.fires && d[0].Confidence != tc.conf {
			t.Errorf("skew %v: confidence = %d, want %d", tc.skew, d[0].Confidence, tc.conf)
		}
	}
}

// A big image cache only matters once the disk is actually under pressure.
func TestImageBloatSignatureNeedsBothDiskAndImages(t *testing.T) {
	cases := []struct {
		name  string
		m     HostMetric
		fires bool
	}{
		{"full disk, many images", HostMetric{DiskUsedPercent: 90, Images: 60}, true},
		{"full disk, few images", HostMetric{DiskUsedPercent: 90, Images: 3}, false},
		{"roomy disk, many images", HostMetric{DiskUsedPercent: 40, Images: 200}, false},
		{"nothing", HostMetric{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Diagnose(Evidence{HostMetrics: map[string]HostMetric{"n": tc.m}})
			if got := hasSig(d, "node.image-bloat"); got != tc.fires {
				t.Errorf("fired = %v, want %v (%+v)", got, tc.fires, d)
			}
		})
	}
}

// A dead runtime explains every workload symptom on that node, so it must
// outrank the pod-level findings it causes.
func TestRuntimeDownOutranksPodSymptoms(t *testing.T) {
	e := Evidence{
		ContainerRuntime: map[string]string{"agent1": "containerd=failed"},
		PodStatuses:      map[string]string{"prod/a": "CrashLoopBackOff"},
	}
	d := Diagnose(e)
	if len(d) < 2 {
		t.Fatalf("expected both signatures, got %+v", d)
	}
	if d[0].SignatureID != "node.runtime-down" {
		t.Errorf("top = %s, want node.runtime-down", d[0].SignatureID)
	}
}

// The healthy-cluster guarantee must survive every signature added here.
func TestHealthyClusterWithHostEvidenceStaysSilent(t *testing.T) {
	e := Evidence{
		K3sService:       map[string]string{"server": "active"},
		ContainerRuntime: map[string]string{"server": "containerd=active"},
		ClockSkew:        map[string]time.Duration{"server": 2 * time.Second},
		HostMetrics:      map[string]HostMetric{"server": {DiskUsedPercent: 46, AvailMemMB: 6000, Images: 12}},
		CoreDNS:          &ReadyRatio{Ready: 1, Desired: 1},
		CertExpiryDays:   364,
		CertSubject:      "client.crt",
	}
	if d := Diagnose(e); len(d) != 0 {
		t.Errorf("healthy cluster produced findings: %+v", d)
	}
}

func hasSig(ds []Diagnosis, id string) bool {
	for _, d := range ds {
		if d.SignatureID == id {
			return true
		}
	}
	return false
}

// TestUnschedulableReasons pins the scheduler phrases that must be recognised.
// The cordon case was missed entirely at first: the remediation already told
// people to uncordon, but no matcher looked for "were unschedulable", and the
// "0/N nodes are available" literal never fired because the real message
// carries a live count like "0/3".
func TestUnschedulableReasons(t *testing.T) {
	realMessages := []string{
		"0/3 nodes are available: 3 Insufficient cpu. preemption: 0/3 nodes are available",
		"0/3 nodes are available: 3 node(s) were unschedulable.",
		"0/3 nodes are available: 3 node(s) had untolerated taint {node.kubernetes.io/unreachable: }",
		"0/2 nodes are available: 2 Insufficient memory.",
		"0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector.",
	}
	for _, msg := range realMessages {
		e := Evidence{PodEvents: map[string][]string{"default/p": {msg}}}
		if !hasSig(Diagnose(e), "pod.pending-sched") {
			t.Errorf("not recognised as unschedulable:\n  %s", msg)
		}
	}

	// An unrelated warning must not fire it.
	e := Evidence{PodEvents: map[string][]string{"default/p": {"Readiness probe failed: HTTP 503"}}}
	if hasSig(Diagnose(e), "pod.pending-sched") {
		t.Error("an unrelated event fired the scheduling signature")
	}
}

// Events outlive their object by about an hour. A PVC deleted minutes ago
// kept reporting "stuck Pending" long after the problem was gone — the same
// staleness bug already fixed for pods, missed for PVCs.
func TestStalePVCEventsAreDropped(t *testing.T) {
	e := &Evidence{PVCEvents: map[string][]string{}}
	parseEvents(`{"items":[{"metadata":{"namespace":"default"},
	  "involvedObject":{"kind":"PersistentVolumeClaim","name":"gone"},
	  "type":"Warning","reason":"ProvisioningFailed",
	  "message":"storageclass.storage.k8s.io \"does-not-exist\" not found"}]}`, e)
	if len(e.PVCEvents) != 1 {
		t.Fatalf("event not recorded: %+v", e.PVCEvents)
	}

	// The PVC list no longer contains it: the object is gone.
	parsePVCs(`{"items":[]}`, e)
	filterStalePVCEvidence(e)
	if len(e.PVCEvents) != 0 {
		t.Errorf("events for a deleted PVC survived: %+v", e.PVCEvents)
	}
	if hasSig(Diagnose(*e), "storage.pvc-pending") {
		t.Error("a deleted PVC still produced a finding")
	}
}

// A PVC that genuinely still exists must keep its events.
func TestLivePVCEventsAreKept(t *testing.T) {
	e := &Evidence{PVCEvents: map[string][]string{}}
	parseEvents(`{"items":[{"metadata":{"namespace":"default"},
	  "involvedObject":{"kind":"PersistentVolumeClaim","name":"real"},
	  "type":"Warning","reason":"ProvisioningFailed",
	  "message":"waiting for a volume to be created"}]}`, e)
	parsePVCs(`{"items":[{"metadata":{"namespace":"default","name":"real"},"status":{"phase":"Pending"}}]}`, e)
	filterStalePVCEvidence(e)
	if len(e.PVCEvents) != 1 {
		t.Fatalf("a live PVC's events were dropped: %+v", e.PVCEvents)
	}
	if !hasSig(Diagnose(*e), "storage.pvc-pending") {
		t.Error("a genuinely pending PVC should still be reported")
	}
}
