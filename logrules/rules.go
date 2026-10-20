package logrules

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Rule is one processing rule: exclude drops a record whose text matches Pattern, mask
// replaces every match of Pattern with Replace (which may use $1, ${name}).
type Rule struct {
	Type    string `mapstructure:"type" json:"type,omitempty"`
	Pattern string `mapstructure:"pattern" json:"pattern"`
	Replace string `mapstructure:"replace" json:"replace,omitempty"`
	Name    string `mapstructure:"name" json:"name,omitempty"`
}

const (
	RuleExclude = "exclude"
	RuleMask    = "mask"

	// DefaultReplacement is what a mask rule without a replacement writes.
	DefaultReplacement = "[REDACTED]"
)

// Ruleset is a compiled list of rules, applied in order.
type Ruleset struct {
	exclude []*regexp.Regexp
	masks   []mask
}

type mask struct {
	name    string
	re      *regexp.Regexp
	replace string
	fn      func(string) string // used instead of replace when set
}

// Compile checks and compiles rules.
func Compile(rules []Rule) (*Ruleset, error) {
	rs := &Ruleset{}
	for i, r := range rules {
		if strings.TrimSpace(r.Pattern) == "" {
			return nil, fmt.Errorf("rule %d: pattern is required", i+1)
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i+1, err)
		}
		switch strings.ToLower(strings.TrimSpace(r.Type)) {
		case RuleExclude:
			rs.exclude = append(rs.exclude, re)
		case RuleMask, "":
			rep := r.Replace
			if rep == "" {
				rep = DefaultReplacement
			}
			rs.masks = append(rs.masks, mask{name: r.Name, re: re, replace: rep})
		default:
			return nil, fmt.Errorf("rule %d: type must be exclude or mask, not %q", i+1, r.Type)
		}
	}
	return rs, nil
}

// MustCompile is Compile for rules known to be valid.
func MustCompile(rules []Rule) *Ruleset {
	rs, err := Compile(rules)
	if err != nil {
		panic(err)
	}
	return rs
}

// Empty reports whether the set has no rules.
func (rs *Ruleset) Empty() bool {
	return rs == nil || (len(rs.exclude) == 0 && len(rs.masks) == 0)
}

// Excluded reports whether an exclude rule matches text.
func (rs *Ruleset) Excluded(text string) bool {
	if rs == nil {
		return false
	}
	for _, re := range rs.exclude {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// Mask applies the mask rules to text.
func (rs *Ruleset) Mask(text string) string {
	if rs == nil {
		return text
	}
	for _, m := range rs.masks {
		if m.fn != nil {
			text = m.re.ReplaceAllStringFunc(text, m.fn)
		} else {
			text = m.re.ReplaceAllString(text, m.replace)
		}
	}
	return text
}

// Apply returns the masked text, or keep=false when the record is excluded. Exclusion is
// checked on the text as written, before masking.
func (rs *Ruleset) Apply(text string) (out string, keep bool) {
	if rs.Excluded(text) {
		return "", false
	}
	return rs.Mask(text), true
}

// ParseExclude compiles a zharp.logs.exclude value (one regex).
func ParseExclude(s string) ([]Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if _, err := regexp.Compile(s); err != nil {
		return nil, fmt.Errorf("exclude: %w", err)
	}
	return []Rule{{Type: RuleExclude, Pattern: s}}, nil
}

// ParseMask reads a zharp.logs.mask value: a JSON list of {"pattern","replace"}, or a single
// such object.
func ParseMask(s string) ([]Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var list []Rule
	if strings.HasPrefix(s, "{") {
		var one Rule
		if err := json.Unmarshal([]byte(s), &one); err != nil {
			return nil, fmt.Errorf("mask: %w", err)
		}
		list = []Rule{one}
	} else if err := json.Unmarshal([]byte(s), &list); err != nil {
		return nil, fmt.Errorf(`mask: want a JSON list like [{"pattern":"...","replace":"..."}]: %w`, err)
	}
	for i := range list {
		list[i].Type = RuleMask
		if _, err := regexp.Compile(list[i].Pattern); err != nil {
			return nil, fmt.Errorf("mask %d: %w", i+1, err)
		}
	}
	return list, nil
}

// Built-in masks, on by default everywhere (DefaultMasks).
const (
	MaskCard       = "credit_card"
	MaskBearer     = "bearer_token"
	MaskJWT        = "jwt"
	MaskAWSKey     = "aws_access_key"
	MaskAWSSecret  = "aws_secret_key"
	MaskPrivateKey = "private_key"
)

// DefaultMaskNames lists the built-in masks in the order they run.
var DefaultMaskNames = []string{MaskPrivateKey, MaskJWT, MaskBearer, MaskAWSKey, MaskAWSSecret, MaskCard}

var (
	// 13–19 digits, optionally grouped by single spaces or dashes, starting like a card number
	// of the major networks (2-6). Kept only when the Luhn check passes.
	cardRe = regexp.MustCompile(`\b[2-6]\d(?:[ -]?\d){11,17}\b`)
	// Authorization: Bearer <token>
	bearerRe = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9\-._~+/]{8,}=*`)
	// header.payload.signature, both JSON parts base64url ("eyJ" is `{"`).
	jwtRe = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]+`)
	// AWS access key IDs (long-term AKIA, temporary ASIA, ...).
	awsKeyRe = regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[A-Z0-9]{16}\b`)
	// aws_secret_access_key = <40 chars>, in env, ini, JSON or logfmt form.
	awsSecretRe = regexp.MustCompile(`(?i)(aws_?secret_?(?:access_?)?key["']?\s*[:=]\s*["']?)[A-Za-z0-9/+]{40}`)
	// -----BEGIN ... PRIVATE KEY----- ... -----END ... PRIVATE KEY----- (or to the end of the
	// record when the END line was cut off).
	privateKeyRe = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z)`)
)

func defaultMask(name string) (mask, bool) {
	switch name {
	case MaskCard:
		return mask{name: name, re: cardRe, fn: func(m string) string {
			if luhn(m) {
				return "[REDACTED:card]"
			}
			return m
		}}, true
	case MaskBearer:
		return mask{name: name, re: bearerRe, replace: "${1} [REDACTED]"}, true
	case MaskJWT:
		return mask{name: name, re: jwtRe, replace: "[REDACTED:jwt]"}, true
	case MaskAWSKey:
		return mask{name: name, re: awsKeyRe, replace: "[REDACTED:aws-key]"}, true
	case MaskAWSSecret:
		return mask{name: name, re: awsSecretRe, replace: "${1}[REDACTED]"}, true
	case MaskPrivateKey:
		return mask{name: name, re: privateKeyRe, replace: "[REDACTED:private-key]"}, true
	}
	return mask{}, false
}

// WithDefaults appends the built-in masks except those named in disabled ("all" disables them
// all). Unknown names are an error, so a typo doesn't silently leave a mask on.
func (rs *Ruleset) WithDefaults(disabled []string) (*Ruleset, error) {
	off := map[string]bool{}
	for _, d := range disabled {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "all" {
			return rs, nil
		}
		if _, ok := defaultMask(d); !ok {
			return nil, fmt.Errorf("unknown default mask %q (known: %s)", d, strings.Join(DefaultMaskNames, ", "))
		}
		off[d] = true
	}
	out := &Ruleset{}
	if rs != nil {
		out.exclude = append(out.exclude, rs.exclude...)
		out.masks = append(out.masks, rs.masks...)
	}
	for _, n := range DefaultMaskNames {
		if !off[n] {
			m, _ := defaultMask(n)
			out.masks = append(out.masks, m)
		}
	}
	return out, nil
}

// luhn checks the digits in s (separators ignored).
func luhn(s string) bool {
	sum, n := 0, 0
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if n%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		n++
	}
	return n >= 13 && sum%10 == 0
}
