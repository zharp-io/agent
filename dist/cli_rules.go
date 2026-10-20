package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zharp-io/agent/logrules"
)

// Log processing rules live in the zharp_logs processor of the logs pipeline: exclude and mask
// rules that apply to every log source, and the built-in masks (card numbers, bearer tokens,
// JWTs, AWS keys, private keys), on unless turned off. Per-container rules come from Docker
// labels and Kubernetes annotations instead.

const rulesProcessor = "zharp_logs"

// logRulesSpec is what the zharp_logs processor block holds.
type logRulesSpec struct {
	Rules          []logrules.Rule
	NoDefaultMasks bool
}

// installRules are the rules given to `install` (--log-exclude, --log-mask, --no-default-masks).
var installRules logRulesSpec

// rulesProcessorBlock is the zharp_logs processor's YAML under processors:.
func rulesProcessorBlock(spec logRulesSpec) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "  %s:\n", rulesProcessor)
	sb.WriteString("    # Built-in masks: card numbers (Luhn-checked), bearer tokens, JWTs, AWS keys, private keys.\n")
	fmt.Fprintf(&sb, "    default_masks: %t\n", !spec.NoDefaultMasks)
	if len(spec.Rules) == 0 {
		sb.WriteString("    rules: []\n")
		return sb.String()
	}
	sb.WriteString("    rules:\n")
	for _, r := range spec.Rules {
		fmt.Fprintf(&sb, "      - type: %s\n", r.Type)
		fmt.Fprintf(&sb, "        pattern: %s\n", configQuote(r.Pattern))
		if r.Type == logrules.RuleMask && r.Replace != "" {
			fmt.Fprintf(&sb, "        replace: %s\n", configQuote(r.Replace))
		}
	}
	return sb.String()
}

// configQuote single-quotes s for YAML and doubles "$" so the collector's ${...} expansion
// leaves regexes and $1 replacements alone.
func configQuote(s string) string {
	return yamlQuote(strings.ReplaceAll(s, "$", "$$"))
}

var logsProcessorsRe = regexp.MustCompile(`(\n[ \t]+logs:[ \t]*\r?\n(?:[ \t]+(?:receivers|exporters):[^\n]*\n)*[ \t]+processors:[ \t]*\[)([^\]]*)\]`)

// ensureLogRules makes sure the config has the zharp_logs processor (with the built-in masks on
// when it has to add it) and that the logs pipeline runs it, right after memory_limiter.
func ensureLogRules(content string) string {
	if !strings.Contains(content, "\n  "+rulesProcessor+":\n") {
		content = strings.Replace(content, "\nexporters:", "\n"+strings.TrimRight(rulesProcessorBlock(logRulesSpec{}), "\n")+"\n\nexporters:", 1)
		content = strings.Replace(content, "\n\n\n", "\n\n", -1)
	}
	return logsProcessorsRe.ReplaceAllStringFunc(content, func(m string) string {
		sub := logsProcessorsRe.FindStringSubmatch(m)
		list := splitList(sub[2])
		if contains(list, rulesProcessor) {
			return m
		}
		at := 0
		for i, p := range list {
			if p == "memory_limiter" {
				at = i + 1
			}
		}
		list = append(list[:at], append([]string{rulesProcessor}, list[at:]...)...)
		return sub[1] + strings.Join(list, ", ") + "]"
	})
}

// readLogRules reads the zharp_logs processor's settings from a config.
func readLogRules(content string) (logRulesSpec, error) {
	var doc struct {
		Processors map[string]struct {
			DefaultMasks *bool `yaml:"default_masks"`
			Rules        []struct {
				Type    string `yaml:"type"`
				Pattern string `yaml:"pattern"`
				Replace string `yaml:"replace"`
			} `yaml:"rules"`
		} `yaml:"processors"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return logRulesSpec{}, err
	}
	p, ok := doc.Processors[rulesProcessor]
	if !ok {
		return logRulesSpec{}, nil
	}
	spec := logRulesSpec{NoDefaultMasks: p.DefaultMasks != nil && !*p.DefaultMasks}
	unescape := func(s string) string { return strings.ReplaceAll(s, "$$", "$") }
	for _, r := range p.Rules {
		spec.Rules = append(spec.Rules, logrules.Rule{Type: r.Type, Pattern: unescape(r.Pattern), Replace: unescape(r.Replace)})
	}
	return spec, nil
}

// writeLogRules replaces the zharp_logs processor block with spec.
func writeLogRules(content string, spec logRulesSpec) string {
	content = ensureLogRules(content)
	content = removeReceiverBlock(content, rulesProcessor)
	return strings.Replace(content, "\nexporters:", "\n"+strings.TrimRight(rulesProcessorBlock(spec), "\n")+"\n\nexporters:", 1)
}

// parseRuleFlags reads --pattern/--replace (or --off/--on) for `add rule <kind>`.
func parseRuleFlags(kind string, args []string) (rule logrules.Rule, masksOn *bool, err error) {
	fs := flag.NewFlagSet("add rule "+kind, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pattern := fs.String("pattern", "", "regular expression (RE2)")
	replace := fs.String("replace", "", "replacement for mask rules ($1 for groups); default "+logrules.DefaultReplacement)
	off := fs.Bool("off", false, "turn the built-in masks off")
	on := fs.Bool("on", false, "turn the built-in masks on")
	if err = fs.Parse(args); err != nil {
		return
	}
	switch kind {
	case "default-masks":
		if *off == *on {
			return rule, nil, fmt.Errorf("give --off or --on")
		}
		v := *on
		return rule, &v, nil
	case logrules.RuleExclude, logrules.RuleMask:
		if *pattern == "" {
			return rule, nil, fmt.Errorf("--pattern is required")
		}
		if kind == logrules.RuleExclude && *replace != "" {
			return rule, nil, fmt.Errorf("--replace only applies to mask rules")
		}
		rule = logrules.Rule{Type: kind, Pattern: *pattern, Replace: *replace}
		if _, err = logrules.Compile([]logrules.Rule{rule}); err != nil {
			return rule, nil, err
		}
		return rule, nil, nil
	}
	return rule, nil, fmt.Errorf("unknown rule type %q (exclude, mask or default-masks)", kind)
}

// cliAddRule is `zharp-collector add rule exclude|mask|default-masks ...`.
func cliAddRule(configFile string, args []string) {
	if len(args) < 1 {
		printRuleUsage()
		os.Exit(1)
	}
	rule, masksOn, err := parseRuleFlags(strings.ToLower(args[0]), args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] %v\n", err)
		printRuleUsage()
		os.Exit(1)
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] %v\n", err)
		os.Exit(1)
	}
	content := ensureLogRules(string(data))
	spec, err := readLogRules(content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Could not read %s: %v\n", configFile, err)
		os.Exit(1)
	}
	switch {
	case masksOn != nil:
		spec.NoDefaultMasks = !*masksOn
		if *masksOn {
			uiOK("Built-in masks on.")
		} else {
			uiOK("Built-in masks off.")
		}
	default:
		dup := false
		for _, r := range spec.Rules {
			dup = dup || (r == rule)
		}
		if !dup {
			spec.Rules = append(spec.Rules, rule)
		}
		uiOK(fmt.Sprintf("Rule added: %s %s", rule.Type, rule.Pattern))
	}
	if err := os.WriteFile(configFile, []byte(writeLogRules(content, spec)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Could not update config: %v\n", err)
		os.Exit(1)
	}
	uiOK("Config updated → " + configFile)
	uiInfo("Restarting service to apply changes...")
	if err := svcRestart(); err != nil {
		uiWarn("Restart failed: " + err.Error())
		uiDimMsg("Restart manually: zharp-collector restart")
	}
	fmt.Println()
}

func printRuleUsage() {
	fmt.Println()
	fmt.Println("  Usage: zharp-collector add rule <exclude|mask|default-masks> [flags]")
	fmt.Println()
	fmt.Println("    exclude --pattern REGEX                 drop log records that match")
	fmt.Println("    mask    --pattern REGEX [--replace S]   rewrite what matches (default " + logrules.DefaultReplacement + "; $1 for groups)")
	fmt.Println("    default-masks --off | --on              built-in masks: " + strings.Join(logrules.DefaultMaskNames, ", "))
	fmt.Println()
}

// installRuleFlags registers --log-exclude, --log-mask and --no-default-masks on fs.
func installRuleFlags(fs *flag.FlagSet) {
	fs.Func("log-exclude", "drop log records matching this regex (repeatable)", func(v string) error {
		r := logrules.Rule{Type: logrules.RuleExclude, Pattern: v}
		if _, err := logrules.Compile([]logrules.Rule{r}); err != nil {
			return err
		}
		installRules.Rules = append(installRules.Rules, r)
		return nil
	})
	fs.Func("log-mask", "replace what matches this regex with "+logrules.DefaultReplacement+" (repeatable)", func(v string) error {
		r := logrules.Rule{Type: logrules.RuleMask, Pattern: v}
		if _, err := logrules.Compile([]logrules.Rule{r}); err != nil {
			return err
		}
		installRules.Rules = append(installRules.Rules, r)
		return nil
	})
	fs.BoolVar(&installRules.NoDefaultMasks, "no-default-masks", false, "don't mask card numbers, tokens and keys in logs (or ZHARP_NO_DEFAULT_MASKS=1)")
}

// applyRuleEnv reads ZHARP_NO_DEFAULT_MASKS, for installs piped through a shell.
func applyRuleEnv() {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ZHARP_NO_DEFAULT_MASKS"))) {
	case "1", "true", "yes", "on":
		installRules.NoDefaultMasks = true
	}
}

// recombineOperator joins multi-line records (stack traces) in a stanza source, with the same
// "auto" rules the Docker receiver and the zharp_logs processor use. source is the field that
// tells streams apart (each file, each systemd unit).
func recombineOperator(source string) string {
	var sb strings.Builder
	sb.WriteString("      # Multi-line: a stack trace becomes one record (logrules auto rules).\n")
	sb.WriteString("      - type: recombine\n")
	sb.WriteString("        combine_field: body\n")
	fmt.Fprintf(&sb, "        is_first_entry: %s\n", yamlQuote(logrules.RecombineFirstEntryExpr("body")))
	fmt.Fprintf(&sb, "        source_identifier: %s\n", source)
	fmt.Fprintf(&sb, "        max_batch_size: %d\n", logrules.DefaultMaxLines)
	fmt.Fprintf(&sb, "        max_log_size: %d\n", logrules.DefaultMaxBytes)
	fmt.Fprintf(&sb, "        force_flush_period: %s\n", logrules.DefaultIdle)
	sb.WriteString("        on_error: send_quiet\n")
	return sb.String()
}
