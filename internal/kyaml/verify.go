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

// rawDoc is one document of a multi-document stream plus the 1-based line in
// the original file where it starts, so issues can point at real lines.
type rawDoc struct {
	text string
	line int
}

// splitDocs separates a multi-document YAML stream.
//
// It walks lines rather than splitting on the substring "\n---", which also
// cut manifests whose block scalar contained a line starting with --- and
// reported bogus errors on a manifest kubectl accepts.
func splitDocs(data []byte) ([]rawDoc, error) {
	var docs []rawDoc
	var cur []string
	start := 1

	flush := func() {
		text := strings.TrimRight(strings.Join(cur, "\n"), " \n")
		// Advance the recorded start past any blank lines the document opened
		// with, so an issue points at real content.
		lead := 0
		for lead < len(cur) && strings.TrimSpace(cur[lead]) == "" {
			lead++
		}
		if text != "" && !allComments(text) {
			docs = append(docs, rawDoc{text: text, line: start + lead})
		}
		cur = nil
	}

	for i, line := range strings.Split(string(data), "\n") {
		if isDocSeparator(line) {
			flush()
			start = i + 2 // the document begins on the line after the separator
			continue
		}
		if cur == nil {
			// first line of a new document
			if len(docs) == 0 && start == 1 {
				start = i + 1
			}
		}
		cur = append(cur, line)
	}
	flush()

	if len(docs) == 0 {
		return nil, fmt.Errorf("no YAML documents found")
	}
	return docs, nil
}

// isDocSeparator reports whether a line is a YAML document separator: --- on
// its own, optionally followed by whitespace or a comment.
//
// \r is trimmed too: a manifest authored on Windows ends its separator line
// with "---\r", and failing to recognise that merged every document into the
// first, so only document 1 was ever validated.
func isDocSeparator(line string) bool {
	t := strings.TrimRight(line, " \t\r")
	if t == "---" {
		return true
	}
	rest, ok := strings.CutPrefix(t, "---")
	if !ok {
		return false
	}
	rest = strings.TrimLeft(rest, " \t")
	return strings.HasPrefix(rest, "#")
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
//  3. schema (offline) — per-kind shape rules for the kinds we know about
//
// Layer 3 is deliberately narrow: it encodes the rules the API server enforces
// that a manifest can violate while still looking structurally fine (a Job with
// restartPolicy: Always, a DaemonSet with replicas). For full schema coverage,
// use VerifyLive, which submits to a real API server.
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
		if err := yaml.Unmarshal([]byte(doc.text), &obj); err != nil {
			res.OK = false
			res.Issues = append(res.Issues, Issue{Line: doc.line, Message: fmt.Sprintf("document %d: invalid YAML: %v", i+1, err)})
			continue
		}
		d := Document{Index: i}
		add := func(field, msg string) {
			res.OK = false
			res.Issues = append(res.Issues, Issue{
				Line:    doc.line,
				Field:   field,
				Message: fmt.Sprintf("document %d: %s", i+1, msg),
			})
		}
		if v, ok := obj["apiVersion"].(string); ok && v != "" {
			d.APIVer = v
		} else {
			add("apiVersion", "missing or non-string apiVersion")
		}
		if v, ok := obj["kind"].(string); ok && v != "" {
			d.Kind = v
		} else {
			add("kind", "missing or non-string kind")
		}
		meta, _ := obj["metadata"].(map[string]interface{})
		if meta == nil {
			add("metadata", "missing metadata")
		} else {
			if name, ok := meta["name"].(string); ok && name != "" {
				d.Name = name
			} else {
				add("metadata.name", "missing or empty metadata.name")
			}
			if ns, ok := meta["namespace"].(string); ok {
				d.Namespace = ns
			}
		}
		for _, iss := range validateKind(d.Kind, obj) {
			add(iss.Field, iss.Message)
		}
		res.Documents = append(res.Documents, d)
	}
	return res
}

// validateKind applies offline shape rules for kinds we know. Unknown kinds
// (CRDs, anything not in KnownKinds) pass: we do not guess at their schema.
func validateKind(kind string, obj map[string]interface{}) []Issue {
	if _, known := KnownKinds[kind]; !known {
		return nil
	}
	var issues []Issue
	bad := func(field, msg string) { issues = append(issues, Issue{Field: field, Message: msg}) }

	spec, _ := obj["spec"].(map[string]interface{})
	needSpec := func() bool {
		if spec == nil {
			bad("spec", fmt.Sprintf("%s requires a spec", kind))
			return false
		}
		return true
	}

	switch kind {
	case "Namespace", "ConfigMap", "Secret":
		// no spec; nothing further to check

	case "Pod":
		if needSpec() {
			issues = append(issues, checkPodSpec("spec", kind, spec)...)
		}

	case "Service":
		if needSpec() {
			// spec.ports is optional in the API: an ExternalName Service must
			// not have ports, and a headless Service used for StatefulSet peer
			// DNS routinely has none. Only require them where they are the
			// point of the object.
			_, hasPorts := spec["ports"]
			typ, _ := spec["type"].(string)
			clusterIP, _ := spec["clusterIP"].(string)
			externalName, _ := spec["externalName"].(string)
			switch {
			case typ == "ExternalName":
				if externalName == "" {
					bad("spec.externalName", "an ExternalName Service requires spec.externalName")
				}
			case clusterIP == "None":
				// headless: ports optional
			case !hasPorts:
				bad("spec.ports", "Service requires spec.ports (unless headless or ExternalName)")
			}
		}

	case "PersistentVolumeClaim":
		if needSpec() {
			if _, ok := spec["accessModes"]; !ok {
				bad("spec.accessModes", "PersistentVolumeClaim requires spec.accessModes")
			}
			if _, ok := spec["resources"]; !ok {
				bad("spec.resources", "PersistentVolumeClaim requires spec.resources")
			}
		}

	case "Ingress":
		if needSpec() {
			_, rules := spec["rules"]
			_, def := spec["defaultBackend"]
			if !rules && !def {
				bad("spec.rules", "Ingress requires spec.rules or spec.defaultBackend")
			}
		}

	case "Deployment", "StatefulSet", "DaemonSet":
		if needSpec() {
			if kind == "DaemonSet" {
				if _, ok := spec["replicas"]; ok {
					bad("spec.replicas", "DaemonSet has no spec.replicas field (one pod per node); the API server rejects it")
				}
			}
			if kind == "StatefulSet" {
				if _, ok := spec["serviceName"]; !ok {
					bad("spec.serviceName", "StatefulSet requires spec.serviceName")
				}
			}
			if _, ok := spec["selector"]; !ok {
				bad("spec.selector", kind+" requires spec.selector")
			}
			issues = append(issues, checkPodTemplate("spec.template", kind, spec, nil)...)
		}

	case "Job":
		if needSpec() {
			if _, ok := spec["selector"]; ok {
				if manual, _ := spec["manualSelector"].(bool); !manual {
					bad("spec.selector", "Job forbids spec.selector unless spec.manualSelector is true; let the controller generate it")
				}
			}
			issues = append(issues, checkPodTemplate("spec.template", kind, spec, jobRestartPolicies)...)
		}

	case "CronJob":
		if needSpec() {
			if _, ok := spec["schedule"]; !ok {
				bad("spec.schedule", "CronJob requires spec.schedule")
			}
			jt, ok := spec["jobTemplate"].(map[string]interface{})
			if !ok {
				bad("spec.jobTemplate", "CronJob requires spec.jobTemplate")
				break
			}
			jobSpec, ok := jt["spec"].(map[string]interface{})
			if !ok {
				bad("spec.jobTemplate.spec", "CronJob requires spec.jobTemplate.spec")
				break
			}
			issues = append(issues, checkPodTemplate("spec.jobTemplate.spec.template", kind, jobSpec, jobRestartPolicies)...)
		}
	}
	return issues
}

// jobRestartPolicies are the only values a Job-managed pod may use; the
// default "Always" is rejected by the API server.
var jobRestartPolicies = []string{"Never", "OnFailure"}

// checkPodTemplate validates parent["template"] as a pod template. When
// allowedRestart is non-nil the pod's restartPolicy must be present and listed.
func checkPodTemplate(path, kind string, parent map[string]interface{}, allowedRestart []string) []Issue {
	tmpl, ok := parent["template"].(map[string]interface{})
	if !ok {
		return []Issue{{Field: path, Message: kind + " requires " + path}}
	}
	podSpec, ok := tmpl["spec"].(map[string]interface{})
	if !ok {
		return []Issue{{Field: path + ".spec", Message: kind + " requires " + path + ".spec"}}
	}
	issues := checkPodSpec(path+".spec", kind, podSpec)
	if allowedRestart != nil {
		rp, _ := podSpec["restartPolicy"].(string)
		if !containsStr(allowedRestart, rp) {
			shown := rp
			if shown == "" {
				shown = "unset (defaults to Always)"
			}
			issues = append(issues, Issue{
				Field: path + ".spec.restartPolicy",
				Message: fmt.Sprintf("%s requires restartPolicy %s, got %s",
					kind, strings.Join(allowedRestart, " or "), shown),
			})
		}
	}
	return issues
}

// checkPodSpec validates the container list of a pod spec.
func checkPodSpec(path, kind string, podSpec map[string]interface{}) []Issue {
	containers, ok := podSpec["containers"].([]interface{})
	if !ok || len(containers) == 0 {
		return []Issue{{Field: path + ".containers", Message: kind + " requires at least one container in " + path + ".containers"}}
	}
	var issues []Issue
	for i, c := range containers {
		cm, ok := c.(map[string]interface{})
		if !ok {
			issues = append(issues, Issue{Field: fmt.Sprintf("%s.containers[%d]", path, i), Message: "container must be a mapping"})
			continue
		}
		if name, _ := cm["name"].(string); name == "" {
			issues = append(issues, Issue{Field: fmt.Sprintf("%s.containers[%d].name", path, i), Message: "container requires a name"})
		}
		if img, _ := cm["image"].(string); img == "" {
			issues = append(issues, Issue{Field: fmt.Sprintf("%s.containers[%d].image", path, i), Message: "container requires an image"})
		}
	}
	return issues
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
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
