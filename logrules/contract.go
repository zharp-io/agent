package logrules

import (
	"strings"
)

// Resource attributes every log, metric and trace carries.
const (
	AttrService     = "service.name"
	AttrEnvironment = "deployment.environment.name"
	AttrVersion     = "service.version"
	AttrSource      = "zharp.source"
)

// Docker labels and container environment variables.
const (
	LabelService   = "zharp.service"
	LabelEnv       = "zharp.env"
	LabelVersion   = "zharp.version"
	LabelSource    = "zharp.source"
	LabelLogs      = "zharp.logs"
	LabelMultiline = "zharp.logs.multiline"
	LabelExclude   = "zharp.logs.exclude"
	LabelMask      = "zharp.logs.mask"

	EnvService = "ZHARP_SERVICE"
	EnvEnv     = "ZHARP_ENV"
	EnvVersion = "ZHARP_VERSION"

	ComposeServiceLabel = "com.docker.compose.service"
)

// Kubernetes pod annotations (prefix + key; per container: prefix + "<container>." + key) and
// the recommended labels used as fallbacks.
const (
	AnnotationPrefix   = "zharp.io/"
	AnnService         = "service"
	AnnEnv             = "env"
	AnnVersion         = "version"
	AnnSource          = "source"
	AnnLogs            = "logs"
	AnnMultiline       = "logs-multiline"
	AnnExclude         = "logs-exclude"
	AnnMask            = "logs-mask"
	K8sLabelName       = "app.kubernetes.io/name"
	K8sLabelVersion    = "app.kubernetes.io/version"
	PodAnnotationsAttr = "k8s.pod.annotations."
	PodLabelsAttr      = "k8s.pod.labels."
)

// Meta is what the contract says about one container: its names and its log settings, raw.
// Compile turns the log settings into rules.
type Meta struct {
	Service, Env, Version, Source string
	// Disabled is set by zharp.logs=false / zharp.io/logs: "false".
	Disabled bool
	// Raw log settings; empty when not given.
	Multiline, Exclude, Mask string
}

// Settings are a container's compiled log settings.
type Settings struct {
	Multiline Multiline
	Rules     *Ruleset // per-source exclude and mask; nil when there are none
}

// Compile parses the log settings. An invalid value is reported and left out; the others
// still apply.
func (m Meta) Compile() (Settings, []error) {
	var errs []error
	s := Settings{Multiline: AutoMultiline}
	if ml, err := ParseMultiline(m.Multiline); err != nil {
		errs = append(errs, err)
	} else {
		s.Multiline = ml
	}
	var rules []Rule
	if r, err := ParseExclude(m.Exclude); err != nil {
		errs = append(errs, err)
	} else {
		rules = append(rules, r...)
	}
	if r, err := ParseMask(m.Mask); err != nil {
		errs = append(errs, err)
	} else {
		rules = append(rules, r...)
	}
	if len(rules) > 0 {
		rs, err := Compile(rules)
		if err != nil {
			errs = append(errs, err)
		} else {
			s.Rules = rs
		}
	}
	return s, errs
}

// ResolveDocker applies the Docker contract: labels win over the container's ZHARP_* env vars,
// which win over the Compose service name, then the container name. The source falls back to
// a guess from the image.
func ResolveDocker(labels map[string]string, env []string, containerName, image string) Meta {
	envs := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "ZHARP_") {
			envs[k] = v
		}
	}
	m := Meta{
		Service:   first(labels[LabelService], envs[EnvService], labels[ComposeServiceLabel], strings.TrimPrefix(containerName, "/")),
		Env:       first(labels[LabelEnv], envs[EnvEnv]),
		Version:   first(labels[LabelVersion], envs[EnvVersion]),
		Source:    first(labels[LabelSource], SourceForImage(image)),
		Disabled:  isFalse(labels[LabelLogs]),
		Multiline: labels[LabelMultiline],
		Exclude:   labels[LabelExclude],
		Mask:      labels[LabelMask],
	}
	return m
}

// K8sPod is what the Kubernetes contract reads about one container of a pod.
type K8sPod struct {
	Annotations map[string]string // pod annotations, full keys (zharp.io/service)
	Labels      map[string]string // pod labels
	Container   string
	Image       string
	// Workload is the deployment, statefulset, daemonset, cronjob or job name, else the pod.
	Workload string
	// Current values already on the resource (an SDK's service.name, say). Annotations win
	// over them; labels and the workload name don't.
	Service, Env, Version, Source string
}

// ResolveK8s applies the Kubernetes contract: a per-container annotation
// (zharp.io/<container>.service) wins over the pod's (zharp.io/service), which wins over what
// the application already set, then app.kubernetes.io/name / version, then the workload name.
func ResolveK8s(p K8sPod) Meta {
	ann := func(key string) string {
		if p.Container != "" {
			if v, ok := p.Annotations[AnnotationPrefix+p.Container+"."+key]; ok {
				return v
			}
		}
		return p.Annotations[AnnotationPrefix+key]
	}
	return Meta{
		Service:   first(ann(AnnService), p.Service, p.Labels[K8sLabelName], p.Workload, p.Container),
		Env:       first(ann(AnnEnv), p.Env),
		Version:   first(ann(AnnVersion), p.Version, p.Labels[K8sLabelVersion]),
		Source:    first(ann(AnnSource), p.Source, SourceForImage(p.Image)),
		Disabled:  isFalse(ann(AnnLogs)),
		Multiline: ann(AnnMultiline),
		Exclude:   ann(AnnExclude),
		Mask:      ann(AnnMask),
	}
}

// imageSources maps well-known image names to the zharp.source they log as. The key matches
// the last path segment of the image repository, exactly or followed by "-" (nginx,
// bitnami/nginx, nginxinc/nginx-unprivileged).
var imageSources = []struct{ image, source string }{
	{"nginx", "nginx"},
	{"openresty", "nginx"},
	{"httpd", "apache"},
	{"apache", "apache"},
	{"postgres", "postgresql"},
	{"postgresql", "postgresql"},
	{"postgis", "postgresql"},
	{"mysql", "mysql"},
	{"mariadb", "mysql"},
	{"percona", "mysql"},
	{"redis", "redis"},
	{"valkey", "redis"},
	{"mongo", "mongodb"},
	{"mongodb", "mongodb"},
	{"node", "node"},
	{"python", "python"},
	{"openjdk", "java"},
	{"eclipse-temurin", "java"},
	{"amazoncorretto", "java"},
	{"ibm-semeru-runtimes", "java"},
	{"tomcat", "java"},
	{"golang", "go"},
}

// SourceForImage guesses zharp.source from an image reference ("" when it isn't a known one).
func SourceForImage(ref string) string {
	name := strings.ToLower(ImageName(ref))
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	for _, s := range imageSources {
		if name == s.image || strings.HasPrefix(name, s.image+"-") {
			return s.source
		}
	}
	return ""
}

// ImageName drops the tag and digest: "nginx:1.27" is "nginx".
func ImageName(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		ref = ref[:i]
	}
	return ref
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func isFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}
