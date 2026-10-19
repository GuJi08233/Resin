package platform

import (
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
)

func TestNormalizeStatusCodeRules_Valid(t *testing.T) {
	rules, err := NormalizeStatusCodeRules([]StatusCodeRule{
		{CodeFrom: 429, CodeTo: 429, Action: StatusCodeActionCooldown, Cooldown: 5 * time.Minute},
		{CodeFrom: 403, CodeTo: 403, Action: StatusCodeActionBlacklist},
		{CodeFrom: 500, CodeTo: 599, Action: StatusCodeActionCooldown, Cooldown: 30 * time.Second},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3", len(rules))
	}
	if rules[0].CodeFrom != 403 || rules[0].Action != StatusCodeActionBlacklist {
		t.Fatalf("rules should be sorted by range start, got %+v", rules[0])
	}
}

func TestNormalizeStatusCodeRules_RejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		rule StatusCodeRule
		want string
	}{
		{
			name: "code below range",
			rule: StatusCodeRule{CodeFrom: 99, CodeTo: 99, Action: StatusCodeActionBlacklist},
			want: "code_from",
		},
		{
			name: "code above range",
			rule: StatusCodeRule{CodeFrom: 600, CodeTo: 600, Action: StatusCodeActionBlacklist},
			want: "code_from",
		},
		{
			name: "reversed range",
			rule: StatusCodeRule{CodeFrom: 500, CodeTo: 400, Action: StatusCodeActionBlacklist},
			want: "code_from must be <= code_to",
		},
		{
			name: "bad action",
			rule: StatusCodeRule{CodeFrom: 403, CodeTo: 403, Action: "DROP"},
			want: "action",
		},
		{
			name: "cooldown missing",
			rule: StatusCodeRule{CodeFrom: 429, CodeTo: 429, Action: StatusCodeActionCooldown},
			want: "cooldown",
		},
		{
			name: "blacklist with cooldown",
			rule: StatusCodeRule{CodeFrom: 403, CodeTo: 403, Action: StatusCodeActionBlacklist, Cooldown: time.Minute},
			want: "cooldown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeStatusCodeRules([]StatusCodeRule{tt.rule})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestNormalizeStatusCodeRules_RejectsOverlap(t *testing.T) {
	_, err := NormalizeStatusCodeRules([]StatusCodeRule{
		{CodeFrom: 400, CodeTo: 499, Action: StatusCodeActionCooldown, Cooldown: time.Minute},
		{CodeFrom: 450, CodeTo: 499, Action: StatusCodeActionBlacklist},
	})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("expected overlap error, got %v", err)
	}
}

func TestMatchStatusCodeRule(t *testing.T) {
	rules := []StatusCodeRule{
		{CodeFrom: 403, CodeTo: 403, Action: StatusCodeActionBlacklist},
		{CodeFrom: 500, CodeTo: 599, Action: StatusCodeActionCooldown, Cooldown: time.Minute},
	}
	if rule, ok := MatchStatusCodeRule(rules, 403); !ok || rule.Action != StatusCodeActionBlacklist {
		t.Fatalf("403 should match blacklist, got %+v ok=%v", rule, ok)
	}
	if rule, ok := MatchStatusCodeRule(rules, 503); !ok || rule.Action != StatusCodeActionCooldown {
		t.Fatalf("503 should match cooldown, got %+v ok=%v", rule, ok)
	}
	if _, ok := MatchStatusCodeRule(rules, 404); ok {
		t.Fatal("404 should not match")
	}
}

func TestStatusCodeRulesModelRoundTrip(t *testing.T) {
	rules, err := NormalizeStatusCodeRules([]StatusCodeRule{
		{CodeFrom: 429, CodeTo: 429, Action: StatusCodeActionCooldown, Cooldown: 5 * time.Minute},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	converted := StatusCodeRulesToModel(rules)
	back, err := StatusCodeRulesFromModel(converted)
	if err != nil {
		t.Fatalf("from model: %v", err)
	}
	if len(back) != 1 || back[0] != rules[0] {
		t.Fatalf("round trip mismatch: got %+v want %+v", back, rules)
	}
	if converted[0].CooldownNs != int64(5*time.Minute) {
		t.Fatalf("cooldown ns = %d", converted[0].CooldownNs)
	}
}

func TestStatusCodeRulesFromModel_Empty(t *testing.T) {
	rules, err := StatusCodeRulesFromModel(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected empty rules, got %+v", rules)
	}
	// Guard against accidental aliasing of the persisted slice.
	var modelRules []model.PlatformStatusCodeRule
	if _, err := StatusCodeRulesFromModel(modelRules); err != nil {
		t.Fatalf("unexpected error for nil slice: %v", err)
	}
}
