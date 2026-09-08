package troubleshoot

import (
	"encoding/json"
	"strings"
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

func keyOf(podName string) string {
	// container key placeholder: first segment of pod name
	parts := strings.SplitN(podName, "-", 2)
	return parts[0]
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
