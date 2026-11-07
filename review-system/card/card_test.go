package card

import (
	"strings"
	"testing"
	"time"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func baseCard() Card {
	return Card{
		Label:          "pilot",
		PRURL:          "https://github.com/teradata-labs/loom/pull/411",
		HeadSHA:        sha,
		Recommendation: "REQUEST_CHANGES",
		Findings: []Finding{
			{ID: "F1", Severity: "MAJOR", Confidence: "HIGH", Bucket: "Finding", Summary: "nil map write | panics"},
		},
		ProposedAction: "post a body-only REQUEST_CHANGES after confirmation",
	}
}

func TestRenderUnknownCostAndTimeStayUnknown(t *testing.T) {
	out, err := Render(baseCard())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"Model cost: unknown", "Operator decision time: unknown", "not a GitHub review event"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$0") {
		t.Error("an unknown cost rendered as a zero amount")
	}
}

func TestRenderKnownCostAndDecisionTime(t *testing.T) {
	c := baseCard()
	cost := 0.42
	arrived := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	decided := arrived.Add(14 * time.Minute)
	c.CostUSD = &cost
	c.Form = Form{ReportReadyAt: &arrived, DecisionAt: &decided}

	out, err := Render(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"$0.4200", "14 min"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

func TestRenderEscapesCellsAndCollapsesLines(t *testing.T) {
	out, err := Render(baseCard())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `nil map write \| panics`) {
		t.Errorf("pipe in summary was not escaped:\n%s", out)
	}
}

func TestRenderEmptyListsSayNothingWasRecorded(t *testing.T) {
	out, err := Render(baseCard())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "None recorded. The absence of a listed gap is not verification.") {
		t.Errorf("empty gaps must not read as verified:\n%s", out)
	}
}

func TestRenderCountsEveryBucket(t *testing.T) {
	c := baseCard()
	c.Findings = append(c.Findings, Finding{ID: "F2", Severity: "MINOR", Confidence: "LOW", Bucket: "Nit", Summary: "style"})
	out, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Finding 1 · Required improvement 0 · Material question 0 · Nit 1") {
		t.Errorf("bucket counts wrong:\n%s", out)
	}
}

func TestValidateRejectsBadCards(t *testing.T) {
	cost := -1.0
	later := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	earlier := later.Add(-time.Hour)
	tests := []struct {
		name    string
		mutate  func(c *Card)
		wantErr string
	}{
		{name: "short head sha", mutate: func(c *Card) { c.HeadSHA = "abc" }, wantErr: "head_sha"},
		{name: "non-github url", mutate: func(c *Card) { c.PRURL = "https://example.com/x" }, wantErr: "pr_url"},
		{name: "unknown recommendation", mutate: func(c *Card) { c.Recommendation = "LGTM" }, wantErr: "recommendation"},
		{name: "empty action", mutate: func(c *Card) { c.ProposedAction = " " }, wantErr: "proposed action"},
		{name: "bad severity", mutate: func(c *Card) { c.Findings[0].Severity = "HIGH" }, wantErr: "severity"},
		{name: "bad bucket", mutate: func(c *Card) { c.Findings[0].Bucket = "Blocker" }, wantErr: "bucket"},
		{name: "duplicate finding id", mutate: func(c *Card) { c.Findings = append(c.Findings, c.Findings[0]) }, wantErr: "unique"},
		{name: "negative cost", mutate: func(c *Card) { c.CostUSD = &cost }, wantErr: "cost_usd"},
		{name: "decision before arrival", mutate: func(c *Card) { c.Form = Form{ReportReadyAt: &later, DecisionAt: &earlier} }, wantErr: "precedes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := baseCard()
			tt.mutate(&c)
			if _, err := Render(c); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecisionMinutesUnknownWithoutBothTimes(t *testing.T) {
	now := time.Now()
	if _, ok := (Form{ReportReadyAt: &now}).DecisionMinutes(); ok {
		t.Fatal("minutes reported without a decision time")
	}
	if _, ok := (Form{}).DecisionMinutes(); ok {
		t.Fatal("minutes reported for an empty form")
	}
}
