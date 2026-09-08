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
	selecting := map[string]bool{"prod/web": true, "prod/api": true, "prod/cache": true}
	parseEndpoints(data, e, selecting)

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
	parseEndpoints(`{"items":[{"metadata":{"namespace":"default","name":"kubernetes"},"subsets":[]}]}`, e,
		map[string]bool{"default/kubernetes": true})
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

// A Service with no selector, a headless Service, and an ExternalName Service
// all legitimately have no endpoints. Flagging them reported a fault on every
// healthy cluster that runs a StatefulSet.
func TestParseEndpointsIgnoresServicesThatSelectNothing(t *testing.T) {
	services := `{"items":[
	  {"metadata":{"namespace":"prod","name":"web"},"spec":{"selector":{"app":"web"},"clusterIP":"10.0.0.1"}},
	  {"metadata":{"namespace":"prod","name":"peers"},"spec":{"selector":{"app":"db"},"clusterIP":"None"}},
	  {"metadata":{"namespace":"prod","name":"ext"},"spec":{"type":"ExternalName"}},
	  {"metadata":{"namespace":"prod","name":"manual"},"spec":{"clusterIP":"10.0.0.9"}}
	]}`
	selecting := map[string]bool{}
	for _, k := range parseSelectingServices(services) {
		selecting[k] = true
	}
	if len(selecting) != 1 || !selecting["prod/web"] {
		t.Fatalf("selecting = %v, want only prod/web", selecting)
	}

	endpoints := `{"items":[
	  {"metadata":{"namespace":"prod","name":"web"},"subsets":[]},
	  {"metadata":{"namespace":"prod","name":"peers"},"subsets":[]},
	  {"metadata":{"namespace":"prod","name":"ext"},"subsets":[]},
	  {"metadata":{"namespace":"prod","name":"manual"},"subsets":[]}
	]}`
	e := &Evidence{}
	parseEndpoints(endpoints, e, selecting)
	if len(e.EmptyEndpoints) != 1 || e.EmptyEndpoints[0] != "prod/web" {
		t.Errorf("EmptyEndpoints = %v, want only the Service that selects pods", e.EmptyEndpoints)
	}
}

// With the Service list unavailable we cannot tell which Services should have
// endpoints, so nothing is reported rather than guessing.
func TestParseEndpointsSilentWithoutServiceList(t *testing.T) {
	e := &Evidence{}
	parseEndpoints(`{"items":[{"metadata":{"namespace":"prod","name":"web"},"subsets":[]}]}`, e, nil)
	if len(e.EmptyEndpoints) != 0 {
		t.Errorf("EmptyEndpoints = %v, want none without a Service list", e.EmptyEndpoints)
	}
}

// --- regressions found by adversarial review ---

// A stopped k3s is the *cause* of an unreachable API server. It must outrank
// "kubeconfig invalid", or the user is sent to check a kubeconfig that is fine.
func TestStoppedServiceOutranksKubeconfigSymptom(t *testing.T) {
	e := Evidence{
		KubeconfigError: "kubectl get nodes failed (exit 1): connection refused",
		K3sService:      map[string]string{"server": "inactive"},
	}
	d := Diagnose(e)
	if len(d) < 2 {
		t.Fatalf("expected both findings, got %+v", d)
	}
	if d[0].SignatureID != "node.notready-k3s-down" {
		t.Errorf("top = %s (%d%%), want node.notready-k3s-down above the kubeconfig symptom; full: %+v",
			d[0].SignatureID, d[0].Confidence, d)
	}
}

// Expired certificates and lost etcd quorum are reasons the API server stops
// answering, so they must also outrank the kubeconfig symptom.
func TestRealCausesOutrankKubeconfigSymptom(t *testing.T) {
	for name, e := range map[string]Evidence{
		"expired certs": {
			KubeconfigError: "kubectl get nodes failed (exit 1): ",
			CertSubject:     "client.crt", CertExpiryDays: -2,
		},
		"etcd below quorum": {
			KubeconfigError: "kubectl get nodes failed (exit 1): ",
			Etcd:            &ReadyRatio{Ready: 1, Desired: 3},
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := Diagnose(e)
			if len(d) == 0 {
				t.Fatal("no findings")
			}
			if d[0].SignatureID == "cluster.kubeconfig" {
				t.Errorf("the symptom outranked the cause: %+v", d)
			}
		})
	}
}

// k3s embeds containerd and never starts the unit, so a leftover stopped
// containerd.service from an earlier kubeadm install is not a fault.
func TestRuntimeDownIgnoresEmbeddedRuntimeOnHealthyK3sNode(t *testing.T) {
	e := Evidence{
		K3sService:       map[string]string{"srv": "active"},
		ContainerRuntime: map[string]string{"srv": "containerd=inactive"},
	}
	if d := Diagnose(e); len(d) != 0 {
		t.Errorf("healthy k3s node with a stray stopped containerd produced %+v", d)
	}
	// But a dead runtime on a node whose Kubernetes service is also unhappy
	// is still worth reporting.
	e.K3sService["srv"] = "failed"
	if !hasSig(Diagnose(e), "node.runtime-down") {
		t.Error("a dead runtime alongside a dead kubelet should still fire")
	}
}

// Empty endpoints are a consequence of pod faults. Ranking them above the pod
// sends the user to debug a selector that is perfectly correct.
func TestEmptyEndpointsRanksBelowItsCause(t *testing.T) {
	e := Evidence{
		EmptyEndpoints: []string{"prod/web"},
		PodStatuses:    map[string]string{"prod/web-1": "CrashLoopBackOff"},
	}
	d := Diagnose(e)
	if len(d) < 2 {
		t.Fatalf("expected both findings, got %+v", d)
	}
	if d[0].SignatureID != "pod.crashloop" {
		t.Errorf("top = %s, want pod.crashloop above network.empty-endpoints; full: %+v", d[0].SignatureID, d)
	}
}

// With no pod-level explanation, a Service with no endpoints really may have a
// wrong selector, and should rank higher than in the correlated case.
func TestEmptyEndpointsRanksHigherWhenUnexplained(t *testing.T) {
	alone := Diagnose(Evidence{EmptyEndpoints: []string{"prod/web"}})
	withCause := Diagnose(Evidence{
		EmptyEndpoints: []string{"prod/web"},
		PodStatuses:    map[string]string{"prod/web-1": "CrashLoopBackOff"},
	})
	var a, b int
	for _, d := range alone {
		if d.SignatureID == "network.empty-endpoints" {
			a = d.Confidence
		}
	}
	for _, d := range withCause {
		if d.SignatureID == "network.empty-endpoints" {
			b = d.Confidence
		}
	}
	if a <= b {
		t.Errorf("unexplained empty endpoints (%d) should outrank explained (%d)", a, b)
	}
}

// A partial gather must never read as a clean bill of health.
func TestPartialEvidenceIsReported(t *testing.T) {
	var e Evidence
	e.probeFailed("pods", "kubectl get pods failed (exit 1)")
	d := Diagnose(e)
	if !hasSig(d, "cluster.partial-evidence") {
		t.Fatalf("a failed probe produced no finding: %+v", d)
	}
	// It must not drown out a real finding.
	e.PodStatuses = map[string]string{"a": "CrashLoopBackOff"}
	d = Diagnose(e)
	if d[0].SignatureID != "pod.crashloop" {
		t.Errorf("top = %s, want the real finding above the incompleteness notice", d[0].SignatureID)
	}
}

// An unparseable pod list must not wipe the evidence gathered from events:
// livePods stays nil so the staleness filter cannot delete everything.
func TestUnparseablePodListDoesNotEraseEvidence(t *testing.T) {
	e := &Evidence{
		PodEvents:       map[string][]string{"prod/web": {"Failed to pull image \"x\": not found"}},
		PodStatuses:     map[string]string{"prod/web": "ImagePullBackOff"},
		ContainerStates: map[string]string{},
	}
	if parsePods("this is not json", e) {
		t.Fatal("parsePods claimed success on garbage")
	}
	filterStalePodEvidence(e)
	if len(e.PodStatuses) == 0 || len(e.PodEvents) == 0 {
		t.Fatal("a failed pod list erased real findings — the cluster would read as healthy")
	}
	if !hasSig(Diagnose(*e), "pod.imagepull") {
		t.Error("the finding was lost")
	}
}

// `systemctl is-active` prints "inactive" for a unit that was never installed,
// and sudo's error text is not a service state at all. Neither is a fault.
func TestServiceStateRecognition(t *testing.T) {
	for _, good := range []string{"active", "inactive", "failed", "activating"} {
		if !isServiceState(good) {
			t.Errorf("%q should be recognised as a service state", good)
		}
	}
	for _, bad := range []string{"sudo: a password is required", "", "bash: systemctl: command not found"} {
		if isServiceState(bad) {
			t.Errorf("%q must not be treated as a service state", bad)
		}
	}
}

// Events outlive the condition they describe by about an hour. A pod that was
// briefly unschedulable while the cluster was starting keeps that event long
// after it is running normally — reporting it as a current fault made a
// healthy cluster look broken.
func TestEventsForRecoveredPodsAreDropped(t *testing.T) {
	e := &Evidence{PodEvents: map[string][]string{}, PodStatuses: map[string]string{}, ContainerStates: map[string]string{}}
	parseEvents(`{"items":[{"metadata":{"namespace":"kube-system"},
	  "involvedObject":{"kind":"Pod","name":"coredns-1"},"type":"Warning","reason":"FailedScheduling",
	  "message":"0/1 nodes are available: 1 node(s) had untolerated taint(s)."}]}`, e)
	if len(e.PodEvents) != 1 {
		t.Fatalf("event not recorded: %+v", e.PodEvents)
	}

	// The pod exists and is now running with every container ready.
	parsePods(`{"items":[{"metadata":{"namespace":"kube-system","name":"coredns-1"},
	  "status":{"phase":"Running","containerStatuses":[{"ready":true,"restartCount":0,"state":{}}]}}]}`, e)
	filterStalePodEvidence(e)

	if len(e.PodEvents) != 0 {
		t.Errorf("events for a recovered pod survived: %+v", e.PodEvents)
	}
	if d := Diagnose(*e); len(d) != 0 {
		t.Errorf("a recovered pod produced findings: %+v", d)
	}
}

// A pod that is still unhealthy keeps its events.
func TestEventsForStillBrokenPodsAreKept(t *testing.T) {
	e := &Evidence{PodEvents: map[string][]string{}, PodStatuses: map[string]string{}, ContainerStates: map[string]string{}}
	parseEvents(`{"items":[{"metadata":{"namespace":"prod"},
	  "involvedObject":{"kind":"Pod","name":"web-1"},"type":"Warning","reason":"FailedScheduling",
	  "message":"0/3 nodes are available: 3 Insufficient cpu."}]}`, e)
	// Present, but not ready.
	parsePods(`{"items":[{"metadata":{"namespace":"prod","name":"web-1"},
	  "status":{"phase":"Pending","containerStatuses":[{"ready":false,"restartCount":0,"state":{}}]}}]}`, e)
	filterStalePodEvidence(e)

	if len(e.PodEvents) != 1 {
		t.Fatalf("a still-unscheduled pod lost its events: %+v", e.PodEvents)
	}
	if !hasSig(Diagnose(*e), "pod.pending-sched") {
		t.Error("the finding was lost")
	}
}

// A pod flapping on a failing liveness probe is momentarily Ready between
// restarts. Treating it as settled would discard the very events that show the
// instability, before it reaches CrashLoopBackOff.
func TestFlappingPodKeepsItsEvents(t *testing.T) {
	e := &Evidence{PodEvents: map[string][]string{}, PodStatuses: map[string]string{}, ContainerStates: map[string]string{}}
	parseEvents(`{"items":[{"metadata":{"namespace":"prod"},
	  "involvedObject":{"kind":"Pod","name":"web-1"},"type":"Warning","reason":"Unhealthy",
	  "message":"Liveness probe failed: HTTP probe failed with statuscode: 500"}]}`, e)

	// Ready right now, but it has restarted 7 times.
	parsePods(`{"items":[{"metadata":{"namespace":"prod","name":"web-1"},
	  "status":{"phase":"Running","containerStatuses":[{"ready":true,"restartCount":7,"state":{}}]}}]}`, e)
	filterStalePodEvidence(e)

	if len(e.PodEvents) != 1 {
		t.Errorf("a restarting pod lost its events: %+v", e.PodEvents)
	}

	// A pod that is ready and has never restarted really has settled.
	e2 := &Evidence{PodEvents: map[string][]string{}, PodStatuses: map[string]string{}, ContainerStates: map[string]string{}}
	parseEvents(`{"items":[{"metadata":{"namespace":"prod"},
	  "involvedObject":{"kind":"Pod","name":"calm-1"},"type":"Warning","reason":"FailedScheduling",
	  "message":"0/3 nodes are available: 3 Insufficient cpu."}]}`, e2)
	parsePods(`{"items":[{"metadata":{"namespace":"prod","name":"calm-1"},
	  "status":{"phase":"Running","containerStatuses":[{"ready":true,"restartCount":0,"state":{}}]}}]}`, e2)
	filterStalePodEvidence(e2)
	if len(e2.PodEvents) != 0 {
		t.Errorf("a genuinely recovered pod kept stale events: %+v", e2.PodEvents)
	}
}

// A NotReady node whose service is running was gathered but never diagnosed —
// doctor reported "no issues detected" on the most common serious fault there
// is.
func TestNotReadyNodeWithHealthyServiceIsDiagnosed(t *testing.T) {
	e := Evidence{
		NodeNotReady: []string{"agent1"},
		K3sService:   map[string]string{"server": "active", "agent1": "active"},
	}
	d := Diagnose(e)
	if !hasSig(d, "node.notready") {
		t.Fatalf("a NotReady node with a running service produced no finding: %+v", d)
	}
	if !strings.Contains(d[0].Remediation, "describe node") {
		t.Errorf("remediation should point at the node's conditions: %s", d[0].Remediation)
	}
}

// When the service is what is down, that signature owns the fault — reporting
// both would send the user to two places for one problem.
func TestNotReadyDefersToAStoppedService(t *testing.T) {
	e := Evidence{
		NodeNotReady: []string{"agent1"},
		K3sService:   map[string]string{"agent1": "inactive"},
	}
	d := Diagnose(e)
	if hasSig(d, "node.notready") {
		t.Errorf("both signatures fired for one fault: %+v", d)
	}
	if !hasSig(d, "node.notready-k3s-down") {
		t.Errorf("the stopped service was not reported: %+v", d)
	}
}

// A pod that runs but never passes readiness serves no traffic and has no
// waiting or terminated reason, so nothing reported it.
func TestNeverReadyPodIsDiagnosed(t *testing.T) {
	e := &Evidence{PodEvents: map[string][]string{}, PodStatuses: map[string]string{}, ContainerStates: map[string]string{}}
	parsePods(`{"items":[{"metadata":{"namespace":"prod","name":"web-1"},
	  "status":{"phase":"Running","containerStatuses":[{"ready":false,"restartCount":0,"state":{}}]}}]}`, e)
	if len(e.NotReadyPods) != 1 {
		t.Fatalf("NotReadyPods = %v, want the running-but-unready pod", e.NotReadyPods)
	}
	if !hasSig(Diagnose(*e), "pod.not-ready") {
		t.Errorf("no finding for a pod that never becomes ready: %+v", Diagnose(*e))
	}
	// A fully ready pod must not be flagged.
	e2 := &Evidence{PodEvents: map[string][]string{}, PodStatuses: map[string]string{}, ContainerStates: map[string]string{}}
	parsePods(`{"items":[{"metadata":{"namespace":"prod","name":"ok-1"},
	  "status":{"phase":"Running","containerStatuses":[{"ready":true,"restartCount":0,"state":{}}]}}]}`, e2)
	if len(e2.NotReadyPods) != 0 {
		t.Errorf("a ready pod was flagged: %v", e2.NotReadyPods)
	}
}

// An unreadable `free` left AvailMemMB at zero, which read as a node with no
// memory left and fired MemoryPressure on a healthy host.
func TestUnmeasuredMemoryDoesNotFireMemoryPressure(t *testing.T) {
	e := Evidence{HostMetrics: map[string]HostMetric{
		"n1": {DiskUsedPercent: 40, AvailMemMB: -1}, // `free` unavailable
	}}
	if hasSig(Diagnose(e), "node.memorypressure") {
		t.Error("an unmeasured memory reading fired MemoryPressure")
	}
	// A real low reading still fires.
	e.HostMetrics["n1"] = HostMetric{DiskUsedPercent: 40, AvailMemMB: 50}
	if !hasSig(Diagnose(e), "node.memorypressure") {
		t.Error("a genuinely low memory reading was not reported")
	}
}

// CoreDNS being down is nearly always a consequence; rank it under a visible
// host-level cause so the user fixes the cause.
func TestCoreDNSRanksUnderItsHostCause(t *testing.T) {
	e := Evidence{
		CoreDNS:    &ReadyRatio{Ready: 0, Desired: 2},
		K3sService: map[string]string{"agent1": "failed"},
	}
	d := Diagnose(e)
	if len(d) < 2 {
		t.Fatalf("expected both findings: %+v", d)
	}
	if d[0].SignatureID == "network.coredns" {
		t.Errorf("CoreDNS outranked the host fault that caused it: %+v", d)
	}
	// With no host cause visible, CoreDNS is the top finding.
	alone := Diagnose(Evidence{CoreDNS: &ReadyRatio{Ready: 0, Desired: 2}})
	if len(alone) == 0 || alone[0].SignatureID != "network.coredns" {
		t.Errorf("unexplained CoreDNS outage should lead: %+v", alone)
	}
}

// Empty endpoints caused by pods that never become ready must rank under that
// cause, not send the user to debug a correct selector.
func TestEmptyEndpointsRanksUnderNotReadyPods(t *testing.T) {
	e := Evidence{
		EmptyEndpoints: []string{"prod/web"},
		NotReadyPods:   []string{"prod/web-1"},
	}
	d := Diagnose(e)
	if len(d) < 2 {
		t.Fatalf("expected both findings: %+v", d)
	}
	if d[0].SignatureID != "pod.not-ready" {
		t.Errorf("top = %s, want pod.not-ready above the Service symptom: %+v", d[0].SignatureID, d)
	}
}
