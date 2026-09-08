// Package kyaml verifies and generates Kubernetes YAML manifests.
package kyaml

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// Issue is a single validation problem found in a manifest.
type Issue struct {
	Line    int    `json:"line"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// Document is one parsed manifest (multi-doc files are split).
type Document struct {
	Index     int    `json:"index"`
	APIVer    string `json:"apiVersion"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// Result is the outcome of verifying a manifest file.
type Result struct {
	OK        bool       `json:"ok"`
	Documents []Document `json:"documents,omitempty"`
	Issues    []Issue    `json:"issues,omitempty"`
}

// splitDocs separates a multi-document YAML stream.
func splitDocs(data []byte) ([]string, error) {
	text := string(data)
	var docs []string
	for _, part := range strings.Split(text, "\n---") {
		trimmed := strings.TrimRight(strings.TrimLeft(part, "\n"), " \n")
		if trimmed == "" || allComments(trimmed) {
			continue
		}
		docs = append(docs, trimmed)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("no YAML documents found")
	}
	return docs, nil
}

func allComments(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") {
			return false
		}
	}
	return true
}

// Verify runs layered validation on manifest bytes:
//  1. syntax  — YAML parses
//  2. structural — apiVersion/kind/metadata.name present and well-formed
func Verify(data []byte) *Result {
	res := &Result{OK: true}
	docs, err := splitDocs(data)
	if err != nil {
		res.OK = false
		res.Issues = append(res.Issues, Issue{Line: 1, Message: err.Error()})
		return res
	}
	for i, doc := range docs {
		var obj map[string]interface{}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			res.OK = false
			res.Issues = append(res.Issues, Issue{Line: 1, Message: fmt.Sprintf("document %d: invalid YAML: %v", i+1, err)})
			continue
		}
		d := Document{Index: i}
		if v, ok := obj["apiVersion"].(string); ok && v != "" {
			d.APIVer = v
		} else {
			res.OK = false
			res.Issues = append(res.Issues, Issue{Field: "apiVersion", Message: fmt.Sprintf("document %d: missing or non-string apiVersion", i+1)})
		}
		if v, ok := obj["kind"].(string); ok && v != "" {
			d.Kind = v
		} else {
			res.OK = false
			res.Issues = append(res.Issues, Issue{Field: "kind", Message: fmt.Sprintf("document %d: missing or non-string kind", i+1)})
		}
		meta, _ := obj["metadata"].(map[string]interface{})
		if meta == nil {
			res.OK = false
			res.Issues = append(res.Issues, Issue{Field: "metadata", Message: fmt.Sprintf("document %d: missing metadata", i+1)})
		} else {
			if name, ok := meta["name"].(string); ok && name != "" {
				d.Name = name
			} else {
				res.OK = false
				res.Issues = append(res.Issues, Issue{Field: "metadata.name", Message: fmt.Sprintf("document %d: missing or empty metadata.name", i+1)})
			}
			if ns, ok := meta["namespace"].(string); ok {
				d.Namespace = ns
			}
		}
		res.Documents = append(res.Documents, d)
	}
	return res
}

// KnownKinds maps kind → default apiVersion used by the generator and structural hints.
var KnownKinds = map[string]string{
	"Namespace":             "v1",
	"ConfigMap":             "v1",
	"Secret":                "v1",
	"Service":               "v1",
	"Pod":                   "v1",
	"PersistentVolumeClaim": "v1",
	"Deployment":            "apps/v1",
	"StatefulSet":           "apps/v1",
	"DaemonSet":             "apps/v1",
	"Job":                   "batch/v1",
	"CronJob":               "batch/v1",
	"Ingress":               "networking.k8s.io/v1",
}
