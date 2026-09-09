package check

import (
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// RegistryConfigPath is where k3s reads registry configuration.
const RegistryConfigPath = "/etc/rancher/k3s/registries.yaml"

// RegistryCheck validates the node's registry configuration.
//
// Everything it looks for fails at image-pull time, on a node, minutes after
// the change that caused it — a malformed file that k3s ignores wholesale, or
// a ca_file naming a certificate that was never copied to this machine. Both
// present as ImagePullBackOff with no hint that the registry configuration is
// involved at all.
type RegistryCheck struct{}

func (c RegistryCheck) ID() string       { return "registry.config" }
func (c RegistryCheck) Name() string     { return "Registry configuration" }
func (c RegistryCheck) Category() string { return "k8s" }

func (c RegistryCheck) Run(ctx Context) Result {
	res := Result{ID: c.ID(), Category: c.Category(), Name: c.Name()}

	out, code, err := ctx.Exec.Run("sudo -n cat " + RegistryConfigPath + " 2>/dev/null")
	if err != nil || code != 0 || strings.TrimSpace(out) == "" {
		// No file is the normal case: a cluster pulling from public
		// registries needs none. Skip rather than pass, so the report does
		// not imply a configuration was checked.
		res.Status = Skip
		res.Summary = "no registries.yaml on this node (pulls go to the default registries)"
		return res
	}

	var parsed struct {
		Mirrors map[string]struct {
			Endpoint []string `json:"endpoint"`
		} `json:"mirrors"`
		Configs map[string]struct {
			Auth *struct {
				Username string `json:"username"`
				Password string `json:"password"`
			} `json:"auth"`
			TLS *struct {
				CAFile             string `json:"ca_file"`
				InsecureSkipVerify bool   `json:"insecure_skip_verify"`
			} `json:"tls"`
		} `json:"configs"`
	}
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		res.Status = Fail
		res.Summary = "registries.yaml does not parse: " + firstLine(err.Error())
		res.Remediation = "k3s ignores the whole file when it cannot parse it, so every private registry on this node " +
			"falls back to an anonymous pull from the default endpoint. Fix the YAML, or rewrite it with " +
			"`k3helper registry apply -t targets.yaml`."
		return res
	}

	var problems, evidence, insecure []string
	for host, cfg := range parsed.Configs {
		if cfg.TLS == nil {
			continue
		}
		if cfg.TLS.InsecureSkipVerify {
			insecure = append(insecure, host)
		}
		if cfg.TLS.CAFile == "" {
			continue
		}
		// The path is on this node, and this check runs on this node, which
		// is the only place the question can be answered.
		if _, code, err := ctx.Exec.Run("test -r " + shellQuote(cfg.TLS.CAFile)); err != nil || code != 0 {
			problems = append(problems, fmt.Sprintf("%s: ca_file %s is missing or unreadable", host, cfg.TLS.CAFile))
		}
	}
	for host := range parsed.Mirrors {
		evidence = append(evidence, host)
	}
	sort.Strings(evidence)
	sort.Strings(problems)
	sort.Strings(insecure)
	res.Evidence = evidence

	switch {
	case len(problems) > 0:
		res.Status = Fail
		res.Summary = strings.Join(problems, "; ")
		res.Remediation = "Copy the CA to that path on this node, or point ca_file at one that exists. Until then every pull " +
			"from that registry fails certificate verification, which surfaces only as ImagePullBackOff."
	case len(insecure) > 0:
		res.Status = Warn
		res.Summary = fmt.Sprintf("%d registr%s configured, TLS verification disabled for: %s",
			len(parsed.Mirrors), pluralY(len(parsed.Mirrors)), strings.Join(insecure, ", "))
		res.Remediation = "insecure_skip_verify accepts any certificate for that registry, including one presented by " +
			"something else. Fine for a throwaway test registry; replace it with ca_file for anything holding images you ship."
	case len(parsed.Mirrors) == 0:
		res.Status = Warn
		res.Summary = "registries.yaml has no mirrors — nothing is being redirected"
		res.Remediation = "A configs block without a matching mirror entry is not applied by k3s to a registry it does not " +
			"otherwise know about. `k3helper registry apply` writes both."
	default:
		res.Status = OK
		res.Summary = fmt.Sprintf("%d registr%s configured (%s)",
			len(parsed.Mirrors), pluralY(len(parsed.Mirrors)), strings.Join(evidence, ", "))
	}
	return res
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
