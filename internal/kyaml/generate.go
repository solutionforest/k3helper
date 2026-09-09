package kyaml

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
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

	// ImagePullSecret names a Secret to add to every generated pod spec, for
	// images that come from a registry the cluster needs credentials for.
	ImagePullSecret string

	// Registry turns a generated Secret into a
	// kubernetes.io/dockerconfigjson pull secret rather than an Opaque one.
	Registry         string
	RegistryUser     string
	RegistryPassword string
	RegistryEmail    string
}

// isPullSecret reports whether the Secret being generated is a registry pull
// secret.
func (p GenParams) isPullSecret() bool { return p.Registry != "" }

// object is a manifest under construction. Building a map and marshalling it
// guarantees well-formed YAML: nothing here does indentation arithmetic.
type object map[string]interface{}

// Generate produces a correct YAML manifest for the given params.
func Generate(p GenParams) (string, error) {
	if p.Name == "" {
		return "", fmt.Errorf("name is required")
	}
	if p.Kind == "" {
		return "", fmt.Errorf("kind is required")
	}
	p.Kind = canonicalKind(p.Kind)
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

	obj := object{
		"apiVersion": apiVer,
		"kind":       p.Kind,
		"metadata":   metadata(p),
	}
	spec, err := specFor(p)
	if err != nil {
		return "", err
	}
	for k, v := range spec {
		obj[k] = v
	}

	out, err := yaml.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", p.Kind, err)
	}
	return string(out), nil
}

func metadata(p GenParams) object {
	meta := object{"name": p.Name}
	if p.Namespace != "" && p.Kind != "Namespace" {
		meta["namespace"] = p.Namespace
	}
	return meta
}

// specFor returns the top-level fields below metadata for the kind. Most kinds
// contribute a single "spec" key; ConfigMap/Secret contribute data instead.
func specFor(p GenParams) (object, error) {
	labels := toIface(p.Labels)

	switch p.Kind {
	case "Namespace":
		return object{}, nil

	case "ConfigMap":
		return object{"data": object{"key": "value"}}, nil

	case "Secret":
		if p.isPullSecret() {
			doc, err := dockerConfigJSON(p)
			if err != nil {
				return nil, err
			}
			// stringData, not data: the API server base64-encodes it, and the
			// manifest stays readable to whoever has to review it. The
			// credential is in there either way — base64 is not encryption,
			// which is why this belongs in a file you apply and delete.
			return object{
				"type":       "kubernetes.io/dockerconfigjson",
				"stringData": object{".dockerconfigjson": doc},
			}, nil
		}
		return object{
			"type":       "Opaque",
			"stringData": object{"key": "value"},
		}, nil

	case "Service":
		return object{"spec": object{
			"selector": labels,
			"ports": []interface{}{object{
				"port":       p.Port,
				"targetPort": p.TargetPort,
				"protocol":   "TCP",
			}},
		}}, nil

	case "Pod":
		return object{"spec": podSpec(p.Name, imageOr(p.Image, "nginx:latest"), p.TargetPort, "", p.ImagePullSecret)}, nil

	case "PersistentVolumeClaim":
		return object{"spec": object{
			"accessModes": []interface{}{"ReadWriteOnce"},
			"resources":   object{"requests": object{"storage": "1Gi"}},
		}}, nil

	case "Deployment":
		return object{"spec": object{
			"replicas": p.Replicas,
			"selector": object{"matchLabels": labels},
			"template": podTemplate(labels, podSpec(p.Name, imageOr(p.Image, "nginx:latest"), p.TargetPort, "", p.ImagePullSecret)),
		}}, nil

	case "StatefulSet":
		return object{"spec": object{
			"replicas":    p.Replicas,
			"serviceName": p.Name,
			"selector":    object{"matchLabels": labels},
			"template":    podTemplate(labels, podSpec(p.Name, imageOr(p.Image, "nginx:latest"), p.TargetPort, "", p.ImagePullSecret)),
		}}, nil

	case "DaemonSet":
		// no replicas: a DaemonSet runs one pod per node and the API rejects
		// spec.replicas as an unknown field.
		return object{"spec": object{
			"selector": object{"matchLabels": labels},
			"template": podTemplate(labels, podSpec(p.Name, imageOr(p.Image, "nginx:latest"), p.TargetPort, "", p.ImagePullSecret)),
		}}, nil

	case "Job":
		// no spec.selector: the API forbids it unless manualSelector is set,
		// and the job controller generates a correct one itself.
		return object{"spec": object{"template": jobPodTemplate(p, labels)}}, nil

	case "CronJob":
		return object{"spec": object{
			"schedule":    "*/5 * * * *",
			"jobTemplate": object{"spec": object{"template": jobPodTemplate(p, labels)}},
		}}, nil

	case "Ingress":
		return object{"spec": object{
			"rules": []interface{}{object{
				"host": p.Name + ".example.com",
				"http": object{"paths": []interface{}{object{
					"path":     "/",
					"pathType": "Prefix",
					"backend": object{"service": object{
						"name": p.Name,
						"port": object{"number": p.Port},
					}},
				}}},
			}},
		}}, nil
	}
	return nil, fmt.Errorf("unsupported kind %q (supported: %s)", p.Kind, supportedKinds())
}

// kindAliases maps the short names kubectl accepts to our canonical kinds.
// Someone who types `kubectl get pvc` every day will type `gen pvc`.
var kindAliases = map[string]string{
	"po": "Pod", "pods": "Pod",
	"svc": "Service", "services": "Service",
	"deploy": "Deployment", "deployments": "Deployment",
	"sts": "StatefulSet", "statefulsets": "StatefulSet",
	"ds": "DaemonSet", "daemonsets": "DaemonSet",
	"ns": "Namespace", "namespaces": "Namespace",
	"cm": "ConfigMap", "configmaps": "ConfigMap",
	"pvc": "PersistentVolumeClaim", "persistentvolumeclaims": "PersistentVolumeClaim",
	"ing": "Ingress", "ingresses": "Ingress",
	"cj": "CronJob", "cronjobs": "CronJob",
	"jobs": "Job", "secrets": "Secret",
}

// canonicalKind resolves a user-supplied kind to its canonical spelling,
// accepting exact names, any casing, and kubectl's short aliases.
func canonicalKind(kind string) string {
	if _, ok := KnownKinds[kind]; ok {
		return kind
	}
	lower := strings.ToLower(strings.TrimSpace(kind))
	if canonical, ok := kindAliases[lower]; ok {
		return canonical
	}
	for k := range KnownKinds {
		if strings.EqualFold(k, kind) {
			return k
		}
	}
	return kind
}

func imageOr(img, def string) string {
	if img == "" {
		return def
	}
	return img
}

// podSpec builds a pod spec with one container. restartPolicy is omitted when
// empty (deployments and friends only accept the default "Always").
func podSpec(name, image string, port int, restartPolicy, pullSecret string) object {
	container := object{"name": name, "image": image}
	if port > 0 {
		container["ports"] = []interface{}{object{"containerPort": port}}
	}
	spec := object{"containers": []interface{}{container}}
	if restartPolicy != "" {
		spec["restartPolicy"] = restartPolicy
	}
	if pullSecret != "" {
		spec["imagePullSecrets"] = []interface{}{object{"name": pullSecret}}
	}
	return spec
}

// dockerConfigJSON renders the .dockerconfigjson body of a pull secret.
//
// The auth field is base64 of "user:password", which is what every registry
// client expects and what makes this a credential to handle carefully rather
// than a config file.
func dockerConfigJSON(p GenParams) (string, error) {
	if p.RegistryUser == "" || p.RegistryPassword == "" {
		return "", fmt.Errorf("a pull secret for %s needs a username and a password", p.Registry)
	}
	entry := map[string]string{
		"username": p.RegistryUser,
		"password": p.RegistryPassword,
		"auth": base64.StdEncoding.EncodeToString(
			[]byte(p.RegistryUser + ":" + p.RegistryPassword)),
	}
	if p.RegistryEmail != "" {
		entry["email"] = p.RegistryEmail
	}
	out, err := json.Marshal(map[string]interface{}{
		"auths": map[string]interface{}{p.Registry: entry},
	})
	if err != nil {
		return "", fmt.Errorf("render dockerconfigjson: %w", err)
	}
	return string(out), nil
}

func podTemplate(labels object, spec object) object {
	return object{
		"metadata": object{"labels": labels},
		"spec":     spec,
	}
}

// jobPodTemplate is a run-to-completion pod template. Jobs require a
// restartPolicy of Never or OnFailure; the "Always" default is rejected.
func jobPodTemplate(p GenParams, labels object) object {
	spec := podSpec(p.Name, imageOr(p.Image, "busybox:latest"), 0, "Never", p.ImagePullSecret)
	containers := spec["containers"].([]interface{})
	containers[0].(object)["command"] = []interface{}{"sh", "-c", "echo done"}
	return podTemplate(labels, spec)
}

func toIface(m map[string]string) object {
	out := object{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func supportedKinds() string {
	kinds := make([]string, 0, len(KnownKinds))
	for k := range KnownKinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ", ")
}
