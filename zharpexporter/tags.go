package zharpexporter

import "go.opentelemetry.io/collector/pdata/pcommon"

// Unified service tagging. Every signal Zharp receives can carry the same three tags —
// service, environment and version — so a log line, a metric point and a span from one
// deployment can be lined up against each other. Logs also carry a source: the technology
// that wrote the line (nginx, postgresql, java, …), which tells the backend how to parse it.
//
// The receivers and the CLI set these as resource attributes; an application instrumented
// with an OTel SDK sets them the same way. A record attribute with the same name is the
// fallback, for pipelines that tag per record instead of per resource.
const (
	AttrServiceName    = "service.name"
	AttrEnvironment    = "deployment.environment.name"
	AttrEnvironmentOld = "deployment.environment" // pre-1.27 semantic conventions
	AttrServiceVersion = "service.version"
	AttrSource         = "zharp.source"
)

// serviceTags are the unified tags of one record.
type serviceTags struct {
	Service string
	Env     string
	Version string
	Source  string
}

// lookupStr returns the first non-empty string value of keys in m.
func lookupStr(m pcommon.Map, keys ...string) string {
	for _, k := range keys {
		if v, ok := m.Get(k); ok {
			if s := v.AsString(); s != "" {
				return s
			}
		}
	}
	return ""
}

// resolveTags reads the unified tags from a resource, falling back to the record's own
// attributes for any tag the resource does not carry. record may be the zero Map.
func resolveTags(resource, record pcommon.Map) serviceTags {
	pick := func(keys ...string) string {
		if s := lookupStr(resource, keys...); s != "" {
			return s
		}
		return lookupStr(record, keys...)
	}
	return serviceTags{
		Service: pick(AttrServiceName),
		Env:     pick(AttrEnvironment, AttrEnvironmentOld),
		Version: pick(AttrServiceVersion),
		Source:  pick(AttrSource),
	}
}

// normaliseEnvAttr makes sure a flattened attribute bag carries the environment under the
// current semantic-convention key, so the backend reads one key whatever SDK sent it.
func normaliseEnvAttr(attrs map[string]any) {
	if attrs == nil {
		return
	}
	if v, ok := attrs[AttrEnvironment]; ok && v != "" {
		return
	}
	if v, ok := attrs[AttrEnvironmentOld]; ok && v != "" {
		attrs[AttrEnvironment] = v
	}
}
