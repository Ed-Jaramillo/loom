// Package pilot defines the frozen input profile for the M1 paired review
// comparison (delivery row W1.1.1). It validates the profile schema. It does
// not run a review and it does not contact GitHub.
package pilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"
)

var (
	idPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	refPattern   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)
)

// Identity names the single pull request under comparison.
type Identity struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	PR    int    `json:"pr"`
}

// Source names the refs that the frozen bundle resolves to exact SHAs.
type Source struct {
	HeadRef string `json:"head_ref"`
	BaseRef string `json:"base_ref"`
}

// Runtime records the execution path. Blocker is set when no verified runtime
// exists. A profile with a blocker is recorded, but it cannot run.
type Runtime struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Blocker string `json:"blocker,omitempty"`
}

// Model records one model setting. Temperature and Seed are requests, not
// observed provider behavior.
type Model struct {
	Provider    string   `json:"provider"`
	Name        string   `json:"name"`
	Temperature *float64 `json:"temperature,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`
}

// Budget holds the caps for one review arm. Zero or absent values are rejected,
// so a missing cap cannot read as "no cap".
type Budget struct {
	MaxWallSeconds int     `json:"max_wall_seconds"`
	MaxToolCalls   int     `json:"max_tool_calls"`
	MaxModelCalls  int     `json:"max_model_calls"`
	MaxCostUSD     float64 `json:"max_cost_usd"`
}

// Profile is the frozen input of one comparison.
type Profile struct {
	ID       string   `json:"id"`
	Identity Identity `json:"identity"`
	Source   Source   `json:"source"`
	Runtime  Runtime  `json:"runtime"`
	Models   []Model  `json:"models"`
	Budget   Budget   `json:"budget"`
}

// Runnable reports whether the profile names a runtime that is available.
func (p Profile) Runnable() bool {
	return p.Runtime.Blocker == ""
}

// Validate checks every required field and returns all problems at once.
func (p Profile) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !idPattern.MatchString(p.ID) {
		add("id %q must match %s", p.ID, idPattern)
	}
	if !ownerPattern.MatchString(p.Identity.Owner) {
		add("identity.owner %q is not a valid GitHub owner", p.Identity.Owner)
	}
	if !repoPattern.MatchString(p.Identity.Repo) {
		add("identity.repo %q is not a valid repository name", p.Identity.Repo)
	}
	if p.Identity.PR <= 0 {
		add("identity.pr must be a positive pull request number")
	}
	if !validRef(p.Source.HeadRef) {
		add("source.head_ref %q is not a valid ref", p.Source.HeadRef)
	}
	if !validRef(p.Source.BaseRef) {
		add("source.base_ref %q is not a valid ref", p.Source.BaseRef)
	}

	if p.Runtime.Name == "" {
		add("runtime.name is required")
	}
	if p.Runtime.Blocker == "" && p.Runtime.Version == "" {
		add("runtime.version is required unless runtime.blocker records why no runtime is available")
	}

	if len(p.Models) == 0 {
		add("models must list at least one model")
	}
	for i, m := range p.Models {
		if m.Provider == "" || m.Name == "" {
			add("models[%d] requires provider and name", i)
		}
		if m.Temperature != nil && (*m.Temperature < 0 || *m.Temperature > 2 || math.IsNaN(*m.Temperature)) {
			add("models[%d].temperature must be in [0, 2]", i)
		}
	}

	b := p.Budget
	if b.MaxWallSeconds <= 0 {
		add("budget.max_wall_seconds must be positive")
	}
	if b.MaxToolCalls <= 0 {
		add("budget.max_tool_calls must be positive")
	}
	if b.MaxModelCalls <= 0 {
		add("budget.max_model_calls must be positive")
	}
	if !(b.MaxCostUSD > 0) || math.IsInf(b.MaxCostUSD, 1) {
		add("budget.max_cost_usd must be a finite positive amount")
	}
	return errors.Join(errs...)
}

func validRef(ref string) bool {
	return refPattern.MatchString(ref) &&
		!strings.HasPrefix(ref, "-") &&
		!strings.Contains(ref, "..")
}

// Load reads a profile file. Unknown fields and trailing data are rejected.
func Load(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, fmt.Errorf("read profile: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var p Profile
	if err := dec.Decode(&p); err != nil {
		return Profile{}, fmt.Errorf("decode profile: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Profile{}, errors.New("decode profile: trailing data after the profile object")
	}
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	return p, nil
}
