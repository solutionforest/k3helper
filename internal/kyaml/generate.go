package kyaml

import (
	"fmt"
	"sort"
	"strings"
)

// GenParams are the inputs for generating a manifest.
type GenParams struct {
	Kind       string
	Name       string
	Namespace  string
	Image      string
	Replicas   int
	Port       int
	TargetPort int // for services; defaults to Port
	Labels     map[string]string
}

// Generate produces a correct YAML manifest for the given params.
func Generate(p GenParams) (string, error) {
	if p.Name == "" {
		return "", fmt.Errorf("name is required")
	}
	if p.Kind == "" {
		return "", fmt.Errorf("kind is required")
	}
	// case-insensitive kind lookup (kubectl convention: "deployment" == "Deployment")
	if _, ok := KnownKinds[p.Kind]; !ok {
		for k := range KnownKinds {
			if strings.EqualFold(k, p.Kind) {
				p.Kind = k
				break
			}
		}
	}
	apiVer, ok := KnownKinds[p.Kind]
	if !ok {
		return "", fmt.Errorf("unsupported kind %q (supported: %s)", p.Kind, supportedKinds())
	}
	if p.Replicas <= 0 {
		p.Replicas = 1
	}
	if p.Port <= 0 {
		p.Port = 80
	}
	if p.TargetPort <= 0 {
		p.TargetPort = p.Port
	}
	if len(p.Labels) == 0 {
		p.Labels = map[string]string{"app": p.Name}
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("apiVersion: %s\nkind: %s\n", apiVer, p.Kind))
	meta := fmt.Sprintf("metadata:\n  name: %s\n", p.Name)
	if p.Namespace != "" && p.Kind != "Namespace" {
		meta = fmt.Sprintf("metadata:\n  name: %s\n  namespace: %s\n", p.Name, p.Namespace)
	}
	b.WriteString(meta)

	switch p.Kind {
	case "Namespace":
		// name only
	case "ConfigMap":
		b.WriteString("data:\n  key: value\n")
	case "Secret":
		b.WriteString("type: Opaque\nstringData:\n  key: value\n")
	case "Service":
		b.WriteString(fmt.Sprintf("spec:\n  selector:\n    app: %s\n  ports:\n    - port: %d\n      targetPort: %d\n      protocol: TCP\n", p.Name, p.Port, p.TargetPort))
	case "PersistentVolumeClaim":
		b.WriteString("spec:\n  accessModes:\n    - ReadWriteOnce\n  resources:\n    requests:\n      storage: 1Gi\n")
	case "Deployment", "StatefulSet", "DaemonSet":
		b.WriteString(fmt.Sprintf("spec:\n  replicas: %d\n", p.Replicas))
		if p.Kind == "StatefulSet" {
			b.WriteString(fmt.Sprintf("  serviceName: %s\n", p.Name))
		}
		b.WriteString(podTemplate(p.Name, imageOr(p.Image, "nginx:latest"), p.TargetPort))
	case "Job":
		b.WriteString(jobPodSpec(p.Name, imageOr(p.Image, "busybox:latest")))
	case "CronJob":
		b.WriteString("spec:\n  schedule: \"*/5 * * * *\"\n  jobTemplate:\n    spec:\n")
		b.WriteString(indent(jobPodSpec(p.Name, imageOr(p.Image, "busybox:latest")), 4))
	case "Ingress":
		b.WriteString(fmt.Sprintf("spec:\n  rules:\n    - host: %s.example.com\n      http:\n        paths:\n          - path: /\n            pathType: Prefix\n            backend:\n              service:\n                name: %s\n                port:\n                  number: %d\n", p.Name, p.Name, p.Port))
	default:
		return "", fmt.Errorf("unsupported kind %q", p.Kind)
	}
	return b.String(), nil
}

func imageOr(img, def string) string {
	if img == "" {
		return def
	}
	return img
}

func podTemplate(name, image string, port int) string {
	return fmt.Sprintf("  selector:\n    matchLabels:\n      app: %s\n  template:\n    metadata:\n      labels:\n        app: %s\n    spec:\n      containers:\n        - name: %s\n          image: %s\n          ports:\n            - containerPort: %d\n", name, name, name, image, port)
}

func jobPodSpec(name, image string) string {
	return fmt.Sprintf("  selector:\n    matchLabels:\n      app: %s\n  template:\n    metadata:\n      labels:\n        app: %s\n    spec:\n      containers:\n        - name: %s\n          image: %s\n          command: [\"sh\", \"-c\", \"echo done\"]\n", name, name, name, image)
}

func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n") + "\n"
}

func supportedKinds() string {
	kinds := make([]string, 0, len(KnownKinds))
	for k := range KnownKinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ", ")
}
