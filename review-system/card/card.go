// Package card renders the compact decision card and the operator time form
// (delivery row W1.1.6). Unknown cost and time stay unknown. They never render
// as zero.
package card

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

var (
	headPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	prURL       = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+/pull/[0-9]+$`)

	recommendations = []string{"APPROVE", "REQUEST_CHANGES", "INCONCLUSIVE", "NOT_REVIEWED"}
	buckets         = []string{"Finding", "Required improvement", "Material question", "Nit", "Out-of-scope", "Dismissed"}
	severities      = []string{"CRITICAL", "MAJOR", "MINOR"}
	confidences     = []string{"HIGH", "MEDIUM", "LOW"}
)

// Finding is one row of the card. Summary is plain text.
type Finding struct {
	ID         string
	Severity   string
	Confidence string
	Bucket     string
	Summary    string
}

// Form is the operator time form. Each field is optional. A missing field stays unknown.
type Form struct {
	ReportReadyAt        *time.Time
	DecisionAt           *time.Time
	ReadMinutes          *int
	FalsePositiveMinutes *int
}

// DecisionMinutes returns whole minutes from report arrival to decision. ok is
// false when either time is absent or the decision precedes the arrival.
func (f Form) DecisionMinutes() (minutes int, ok bool) {
	if f.ReportReadyAt == nil || f.DecisionAt == nil {
		return 0, false
	}
	d := f.DecisionAt.Sub(*f.ReportReadyAt)
	if d < 0 {
		return 0, false
	}
	return int(d.Round(time.Minute) / time.Minute), true
}

// Card is the first view of one review. Recommendation is the internal
// code_recommendation. It is not a GitHub review event.
type Card struct {
	Label             string
	PRURL             string
	HeadSHA           string
	Recommendation    string
	Findings          []Finding
	Gaps              []string
	PriorRoundChanges []string
	ProposedAction    string
	CostUSD           *float64
	Form              Form
}

// Validate checks the card fields before rendering.
func (c Card) Validate() error {
	var errs []error
	if strings.TrimSpace(c.Label) == "" {
		errs = append(errs, errors.New("label is required"))
	}
	if !prURL.MatchString(c.PRURL) {
		errs = append(errs, fmt.Errorf("pr_url %q must be a github.com pull request URL", c.PRURL))
	}
	if !headPattern.MatchString(c.HeadSHA) {
		errs = append(errs, fmt.Errorf("head_sha %q must be a full 40-character commit SHA", c.HeadSHA))
	}
	if !contains(recommendations, c.Recommendation) {
		errs = append(errs, fmt.Errorf("recommendation %q is not one of %v", c.Recommendation, recommendations))
	}
	if strings.TrimSpace(c.ProposedAction) == "" {
		errs = append(errs, errors.New("proposed action is required"))
	}
	seen := map[string]bool{}
	for i, f := range c.Findings {
		if f.ID == "" || seen[f.ID] {
			errs = append(errs, fmt.Errorf("findings[%d]: id must be present and unique", i))
		}
		seen[f.ID] = true
		if !contains(severities, f.Severity) {
			errs = append(errs, fmt.Errorf("findings[%d]: severity %q is invalid", i, f.Severity))
		}
		if !contains(confidences, f.Confidence) {
			errs = append(errs, fmt.Errorf("findings[%d]: confidence %q is invalid", i, f.Confidence))
		}
		if !contains(buckets, f.Bucket) {
			errs = append(errs, fmt.Errorf("findings[%d]: bucket %q is invalid", i, f.Bucket))
		}
		if strings.TrimSpace(f.Summary) == "" {
			errs = append(errs, fmt.Errorf("findings[%d]: summary is required", i))
		}
	}
	if c.CostUSD != nil && (*c.CostUSD < 0 || math.IsNaN(*c.CostUSD) || math.IsInf(*c.CostUSD, 0)) {
		errs = append(errs, errors.New("cost_usd must be a finite non-negative amount"))
	}
	if c.Form.ReportReadyAt != nil && c.Form.DecisionAt != nil && c.Form.DecisionAt.Before(*c.Form.ReportReadyAt) {
		errs = append(errs, errors.New("decision time precedes report arrival"))
	}
	return errors.Join(errs...)
}

// Render returns the card as Markdown. It fails when validation fails.
func Render(c Card) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### Decision card: %s\n\n", oneLine(c.Label))
	fmt.Fprintf(&b, "- PR: %s\n", c.PRURL)
	fmt.Fprintf(&b, "- Head: `%s`\n", c.HeadSHA)
	fmt.Fprintf(&b, "- Internal recommendation: `%s` (not a GitHub review event)\n", c.Recommendation)
	fmt.Fprintf(&b, "- Proposed action: %s\n", oneLine(c.ProposedAction))
	fmt.Fprintf(&b, "- Model cost: %s\n", formatCost(c.CostUSD))
	fmt.Fprintf(&b, "- Operator decision time: %s\n\n", formatMinutes(c.Form))

	fmt.Fprintf(&b, "#### Findings (%d)\n\n", len(c.Findings))
	counts := make(map[string]int, len(buckets))
	for _, f := range c.Findings {
		counts[f.Bucket]++
	}
	parts := make([]string, 0, len(buckets))
	for _, bk := range buckets {
		parts = append(parts, fmt.Sprintf("%s %d", bk, counts[bk]))
	}
	fmt.Fprintf(&b, "By bucket: %s\n\n", strings.Join(parts, " · "))
	if len(c.Findings) > 0 {
		b.WriteString("| ID | Severity | Confidence | Bucket | Summary |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, f := range c.Findings {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
				oneLine(f.ID), f.Severity, f.Confidence, f.Bucket, escapeCell(f.Summary))
		}
		b.WriteString("\n")
	}

	b.WriteString("#### Verification gaps\n\n")
	writeList(&b, c.Gaps, "None recorded. The absence of a listed gap is not verification.")

	b.WriteString("\n#### Changes since prior round\n\n")
	writeList(&b, c.PriorRoundChanges, "None recorded (first round, or not compared).")
	return b.String(), nil
}

func writeList(b *strings.Builder, items []string, empty string) {
	if len(items) == 0 {
		b.WriteString(empty + "\n")
		return
	}
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", oneLine(it))
	}
}

func formatCost(cost *float64) string {
	if cost == nil {
		return "unknown"
	}
	return fmt.Sprintf("$%.4f", *cost)
}

func formatMinutes(f Form) string {
	m, ok := f.DecisionMinutes()
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%d min", m)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func escapeCell(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", `\|`)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
