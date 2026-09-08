package troubleshoot

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type nodeInfo struct {
	Name       string
	Conditions []struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	}
}

func parseNodes(data string) ([]nodeInfo, error) {
	var raw struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, err
	}
	var out []nodeInfo
	for _, it := range raw.Items {
		n := nodeInfo{Name: it.Metadata.Name}
		n.Conditions = it.Status.Conditions
		out = append(out, n)
	}
	return out, nil
}

func parsePods(data string, e *Evidence) {
	if e.livePods == nil {
		e.livePods = map[string]bool{}
	}
	var raw struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase             string `json:"phase"`
				Reason            string `json:"reason"`
				ContainerStatuses []struct {
					State struct {
						Waiting *struct {
							Reason string `json:"reason"`
						} `json:"waiting"`
						Terminated *struct {
							Reason   string `json:"reason"`
							ExitCode int    `json:"exitCode"`
						} `json:"terminated"`
					} `json:"state"`
					LastState struct {
						Terminated *struct {
							Reason   string `json:"reason"`
							ExitCode int    `json:"exitCode"`
						} `json:"terminated"`
					} `json:"lastState"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return
	}
	for _, it := range raw.Items {
		key := it.Metadata.Namespace + "/" + it.Metadata.Name
		e.livePods[key] = true
		reason := it.Status.Reason
		if reason == "" {
			for _, cs := range it.Status.ContainerStatuses {
				// OOMKilled often appears in lastState.terminated while the
				// container is waiting to restart
				if cs.LastState.Terminated != nil && cs.LastState.Terminated.Reason == "OOMKilled" {
					e.ContainerStates[key] = "OOMKilled"
					reason = "OOMKilled"
				}
				if cs.State.Waiting != nil && isProblemReason(cs.State.Waiting.Reason) {
					reason = cs.State.Waiting.Reason
				}
				if cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled" {
					e.ContainerStates[key] = "OOMKilled"
					reason = "OOMKilled"
				}
			}
		}
		if isProblemReason(reason) {
			e.PodStatuses[key] = reason
		}
	}
}

// parseReadyRatio reads kubectl jsonpath output of the form "2/2". An empty
// readyReplicas renders as "/2", which means zero ready.
func parseReadyRatio(out string) (ready, desired int, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(out), "/", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] != "" {
		r, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, 0, false
		}
		ready = r
	}
	d, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return ready, d, true
}

// `k3s certificate check` writes one line per certificate to stderr, e.g.
//
//	time="..." level=info msg="client-kube-apiserver.crt: certificate
//	system:apiserver (ClientAuth) is ok, expires at 2027-09-08T08:45:21Z"
//
// The date and the subject are matched separately because the subject form
// varies and a combined pattern lets the engine skip past it. Only the date
// prefix of the timestamp is captured; the time of day does not change which
// certificate expires first at day granularity.
var (
	certDateRe = regexp.MustCompile(`(?i)expires(?: at| on)?[:\s]+([0-9]{4}-[0-9]{2}-[0-9]{2})`)
	certFileRe = regexp.MustCompile(`([A-Za-z0-9_-]+\.crt)`)
	certCNRe   = regexp.MustCompile(`CN=[^,\s"]+`)
)

// certSubject names the certificate a line refers to, preferring an explicit
// CN, then the .crt filename k3s reports, then a generic label.
func certSubject(line string) string {
	if cn := certCNRe.FindString(line); cn != "" {
		return cn
	}
	if f := certFileRe.FindString(line); f != "" {
		return f
	}
	return "k3s certificate"
}

// parseCertExpiry returns days until the soonest-expiring certificate and its
// subject. Returns (0, "") when nothing parses, which the signature reads as
// "no cert evidence" rather than "expired today".
func parseCertExpiry(out string, now time.Time) (int, string) {
	soonest := 0
	subject := ""
	found := false
	for _, line := range strings.Split(out, "\n") {
		m := certDateRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		t, err := time.Parse("2006-01-02", m[1])
		if err != nil {
			continue
		}
		days := int(t.Sub(now).Hours() / 24)
		if found && days >= soonest {
			continue
		}
		soonest, found = days, true
		subject = certSubject(line)
	}
	if !found {
		return 0, ""
	}
	return soonest, subject
}

// parseEndpoints records Services whose endpoint object has no ready
// addresses — the Service exists and resolves, but nothing serves it.
func parseEndpoints(data string, e *Evidence) {
	var raw struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Subsets []struct {
				Addresses []struct {
					IP string `json:"ip"`
				} `json:"addresses"`
			} `json:"subsets"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return
	}
	for _, it := range raw.Items {
		// These are control-plane endpoints managed outside the Service
		// mechanism; they legitimately have no pod-backed addresses.
		if it.Metadata.Namespace == "default" && it.Metadata.Name == "kubernetes" {
			continue
		}
		ready := 0
		for _, s := range it.Subsets {
			ready += len(s.Addresses)
		}
		if ready == 0 {
			e.EmptyEndpoints = append(e.EmptyEndpoints, it.Metadata.Namespace+"/"+it.Metadata.Name)
		}
	}
	sort.Strings(e.EmptyEndpoints)
}

func isProblemReason(r string) bool {
	switch r {
	case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "Evicted", "OOMKilled", "CreateContainerError", "RunContainerError", "InvalidImageName":
		return true
	}
	return false
}

func parseEvents(data string, e *Evidence) {
	var raw struct {
		Items []struct {
			ObjectMeta struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			InvolvedObject struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
			Type    string `json:"type"`
			Count   int    `json:"count"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return
	}
	for _, it := range raw.Items {
		if it.Type != "Warning" {
			continue
		}
		key := it.ObjectMeta.Namespace + "/" + it.InvolvedObject.Name
		switch it.InvolvedObject.Kind {
		case "Pod":
			e.PodEvents[key] = append(e.PodEvents[key], it.Message)
		case "PersistentVolumeClaim":
			e.PVCEvents[key] = append(e.PVCEvents[key], it.Message)
		}
	}
}

func parsePVCs(data string, e *Evidence) {
	var raw struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return
	}
	for _, it := range raw.Items {
		if it.Status.Phase == "Pending" {
			key := it.Metadata.Namespace + "/" + it.Metadata.Name
			if _, ok := e.PVCEvents[key]; !ok {
				e.PVCEvents[key] = []string{"PVC status: Pending"}
			}
		}
	}
}
