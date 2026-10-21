package views

import (
	"context"
	"strings"
	"testing"
)

// A command the platform switched off shows as Off, with each reason under
// it, so the person can find what to open and save again.
func TestRuleCardShowsWhyACommandIsOff(t *testing.T) {
	var b strings.Builder
	err := ruleCardContent(RuleCardView{
		Canonical:      "move it",
		DisplayPhrase:  "move it",
		IsUser:         true,
		BlockedReasons: []string{"Services' 'Update an issue' changed; review the command in Settings"},
	}).Render(context.Background(), &b)
	if err != nil {
		t.Fatal(err)
	}
	html := b.String()
	if !strings.Contains(html, ">Off<") || !strings.Contains(html, "review the command in Settings") {
		t.Fatalf("expected the Off badge and the reason, got %s", html)
	}

	b.Reset()
	if err := ruleCardContent(RuleCardView{Canonical: "x", DisplayPhrase: "x", IsUser: true}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), ">Off<") {
		t.Fatal("a live command shows no Off badge")
	}
}
