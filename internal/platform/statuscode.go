package platform

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/model"
)

// Status code rule bounds and limits.
const (
	MinStatusCode      = 100
	MaxStatusCode      = 599
	MaxStatusCodeRules = 64
)

// StatusCodeAction is the platform-scoped action taken when an upstream
// response matches a configured status code rule.
type StatusCodeAction string

const (
	StatusCodeActionCooldown  StatusCodeAction = "COOLDOWN"
	StatusCodeActionBlacklist StatusCodeAction = "BLACKLIST"
)

// IsValid reports whether the action is one of the supported values.
func (a StatusCodeAction) IsValid() bool {
	return a == StatusCodeActionCooldown || a == StatusCodeActionBlacklist
}

// ParseStatusCodeAction normalizes and validates a raw action string.
func ParseStatusCodeAction(raw string) (StatusCodeAction, bool) {
	action := StatusCodeAction(strings.ToUpper(strings.TrimSpace(raw)))
	return action, action.IsValid()
}

// StatusCodeRule maps an inclusive HTTP status code range to an action.
// Cooldown is only meaningful for StatusCodeActionCooldown.
type StatusCodeRule struct {
	CodeFrom int
	CodeTo   int
	Action   StatusCodeAction
	Cooldown time.Duration
}

// Matches reports whether code falls inside the rule's inclusive range.
func (r StatusCodeRule) Matches(code int) bool {
	return code >= r.CodeFrom && code <= r.CodeTo
}

// MatchStatusCodeRule returns the first rule matching code.
func MatchStatusCodeRule(rules []StatusCodeRule, code int) (StatusCodeRule, bool) {
	for _, rule := range rules {
		if rule.Matches(code) {
			return rule, true
		}
	}
	return StatusCodeRule{}, false
}

// NormalizeStatusCodeRules validates and returns a canonical copy of rules.
// Rules are sorted by range start; overlapping ranges are rejected because
// they would make the action ambiguous.
func NormalizeStatusCodeRules(rules []StatusCodeRule) ([]StatusCodeRule, error) {
	if len(rules) == 0 {
		return []StatusCodeRule{}, nil
	}
	if len(rules) > MaxStatusCodeRules {
		return nil, fmt.Errorf("status_code_rules: at most %d rules are allowed", MaxStatusCodeRules)
	}

	normalized := make([]StatusCodeRule, len(rules))
	for i, rule := range rules {
		action, ok := ParseStatusCodeAction(string(rule.Action))
		if !ok {
			return nil, fmt.Errorf(
				"status_code_rules[%d].action: must be %s or %s",
				i, StatusCodeActionCooldown, StatusCodeActionBlacklist,
			)
		}
		if rule.CodeFrom < MinStatusCode || rule.CodeFrom > MaxStatusCode {
			return nil, fmt.Errorf(
				"status_code_rules[%d].code_from: must be between %d and %d",
				i, MinStatusCode, MaxStatusCode,
			)
		}
		if rule.CodeTo < MinStatusCode || rule.CodeTo > MaxStatusCode {
			return nil, fmt.Errorf(
				"status_code_rules[%d].code_to: must be between %d and %d",
				i, MinStatusCode, MaxStatusCode,
			)
		}
		if rule.CodeFrom > rule.CodeTo {
			return nil, fmt.Errorf("status_code_rules[%d]: code_from must be <= code_to", i)
		}

		switch action {
		case StatusCodeActionCooldown:
			if rule.Cooldown <= 0 {
				return nil, fmt.Errorf("status_code_rules[%d].cooldown: must be > 0 for COOLDOWN", i)
			}
		case StatusCodeActionBlacklist:
			if rule.Cooldown != 0 {
				return nil, fmt.Errorf("status_code_rules[%d].cooldown: must be empty for BLACKLIST", i)
			}
		}

		normalized[i] = StatusCodeRule{
			CodeFrom: rule.CodeFrom,
			CodeTo:   rule.CodeTo,
			Action:   action,
			Cooldown: rule.Cooldown,
		}
	}

	sort.SliceStable(normalized, func(i, j int) bool {
		if normalized[i].CodeFrom == normalized[j].CodeFrom {
			return normalized[i].CodeTo < normalized[j].CodeTo
		}
		return normalized[i].CodeFrom < normalized[j].CodeFrom
	})
	for i := 1; i < len(normalized); i++ {
		if normalized[i].CodeFrom <= normalized[i-1].CodeTo {
			return nil, fmt.Errorf(
				"status_code_rules: ranges [%d-%d] and [%d-%d] overlap",
				normalized[i-1].CodeFrom, normalized[i-1].CodeTo,
				normalized[i].CodeFrom, normalized[i].CodeTo,
			)
		}
	}
	return normalized, nil
}

// StatusCodeRulesFromModel converts persisted rules into runtime rules.
func StatusCodeRulesFromModel(rules []model.PlatformStatusCodeRule) ([]StatusCodeRule, error) {
	converted := make([]StatusCodeRule, len(rules))
	for i, rule := range rules {
		converted[i] = StatusCodeRule{
			CodeFrom: rule.CodeFrom,
			CodeTo:   rule.CodeTo,
			Action:   StatusCodeAction(rule.Action),
			Cooldown: time.Duration(rule.CooldownNs),
		}
	}
	return NormalizeStatusCodeRules(converted)
}

// StatusCodeRulesToModel converts runtime rules into persisted rules.
func StatusCodeRulesToModel(rules []StatusCodeRule) []model.PlatformStatusCodeRule {
	if len(rules) == 0 {
		return []model.PlatformStatusCodeRule{}
	}
	converted := make([]model.PlatformStatusCodeRule, len(rules))
	for i, rule := range rules {
		converted[i] = model.PlatformStatusCodeRule{
			CodeFrom:   rule.CodeFrom,
			CodeTo:     rule.CodeTo,
			Action:     string(rule.Action),
			CooldownNs: int64(rule.Cooldown),
		}
	}
	return converted
}
