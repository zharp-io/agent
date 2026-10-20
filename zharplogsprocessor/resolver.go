package zharplogsprocessor

import (
	"context"
	"strings"
	"sync"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"

	"github.com/zharp-io/agent/logrules"
)

// Resource attributes the k8sattributes processor and the filelog container parser set.
const (
	attrPodUID      = "k8s.pod.uid"
	attrPodName     = "k8s.pod.name"
	attrContainer   = "k8s.container.name"
	attrImage       = "container.image.name"
	annotationsAttr = logrules.PodAnnotationsAttr + logrules.AnnotationPrefix // k8s.pod.annotations.zharp.io/
)

// workloadAttrs name the pod's owner, most specific first.
var workloadAttrs = []string{
	"k8s.deployment.name", "k8s.statefulset.name", "k8s.daemonset.name",
	"k8s.cronjob.name", "k8s.job.name", "k8s.replicaset.name",
}

// resolver applies the Kubernetes annotation contract and the default environment to a
// resource.
type resolver struct {
	cfg    *Config
	logger *zap.Logger

	mu     sync.Mutex
	cache  map[string]logrules.Settings
	warned map[string]bool
}

func newResolver(cfg *Config, logger *zap.Logger) *resolver {
	return &resolver{cfg: cfg, logger: logger, cache: map[string]logrules.Settings{}, warned: map[string]bool{}}
}

// resolved is what the contract says about one resource's logs.
type resolved struct {
	k8s      bool // the resource is a pod's container
	disabled bool // zharp.io/logs: "false"
	settings logrules.Settings
	key      string // identifies the container, for joining lines
}

// apply sets service.name, deployment.environment.name, service.version and zharp.source on
// res following the contract, and removes the zharp.io annotations it read (a mask pattern is
// not something to export).
func (r *resolver) apply(res pcommon.Resource) resolved {
	attrs := res.Attributes()
	var out resolved
	if r.cfg.Kubernetes.Enabled && isPod(attrs) {
		pod := logrules.K8sPod{
			Annotations: map[string]string{},
			Labels: map[string]string{
				logrules.K8sLabelName:    str(attrs, logrules.PodLabelsAttr+logrules.K8sLabelName),
				logrules.K8sLabelVersion: str(attrs, logrules.PodLabelsAttr+logrules.K8sLabelVersion),
			},
			Container: str(attrs, attrContainer),
			Image:     str(attrs, attrImage),
			Service:   str(attrs, logrules.AttrService),
			Env:       str(attrs, logrules.AttrEnvironment),
			Version:   str(attrs, logrules.AttrVersion),
			Source:    str(attrs, logrules.AttrSource),
		}
		for _, k := range workloadAttrs {
			if v := str(attrs, k); v != "" {
				pod.Workload = v
				break
			}
		}
		attrs.RemoveIf(func(k string, v pcommon.Value) bool {
			if strings.HasPrefix(k, annotationsAttr) {
				pod.Annotations[strings.TrimPrefix(k, logrules.PodAnnotationsAttr)] = v.AsString()
				return true
			}
			return false
		})
		// An SDK's "unknown_service:java" is no name at all.
		if strings.HasPrefix(pod.Service, "unknown_service") {
			pod.Service = ""
		}
		meta := logrules.ResolveK8s(pod)
		putIf(attrs, logrules.AttrService, meta.Service)
		putIf(attrs, logrules.AttrEnvironment, meta.Env)
		putIf(attrs, logrules.AttrVersion, meta.Version)
		putIf(attrs, logrules.AttrSource, meta.Source)
		out = resolved{k8s: true, disabled: meta.Disabled, settings: r.compile(meta),
			key: first(str(attrs, attrPodUID), str(attrs, attrPodName)) + "/" + pod.Container}
	}
	if r.cfg.Environment != "" {
		if _, ok := attrs.Get(logrules.AttrEnvironment); !ok {
			attrs.PutStr(logrules.AttrEnvironment, r.cfg.Environment)
		}
	}
	return out
}

// compile turns a pod's raw log settings into rules, once per distinct set of values.
func (r *resolver) compile(meta logrules.Meta) logrules.Settings {
	if meta.Multiline == "" {
		meta.Multiline = r.cfg.Kubernetes.Multiline
	}
	key := meta.Multiline + "\x00" + meta.Exclude + "\x00" + meta.Mask
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.cache[key]; ok {
		return s
	}
	s, errs := meta.Compile()
	if len(errs) > 0 && !r.warned[key] {
		r.warned[key] = true
		for _, err := range errs {
			r.logger.Warn("Ignoring a zharp.io/logs-* annotation that could not be used",
				zap.String("service", meta.Service), zap.Error(err))
		}
	}
	if len(r.cache) > 4096 {
		r.cache = map[string]logrules.Settings{}
	}
	r.cache[key] = s
	return s
}

func (r *resolver) processMetrics(_ context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		r.apply(md.ResourceMetrics().At(i).Resource())
	}
	return md, nil
}

func (r *resolver) processTraces(_ context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		r.apply(td.ResourceSpans().At(i).Resource())
	}
	return td, nil
}

func isPod(attrs pcommon.Map) bool {
	return str(attrs, attrPodUID) != "" || str(attrs, attrPodName) != ""
}

func str(attrs pcommon.Map, k string) string {
	if v, ok := attrs.Get(k); ok {
		return v.AsString()
	}
	return ""
}

func putIf(attrs pcommon.Map, k, v string) {
	if v != "" {
		attrs.PutStr(k, v)
	}
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
