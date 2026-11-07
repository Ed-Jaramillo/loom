package pilot

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validProfile() Profile {
	temp := 0.0
	return Profile{
		ID:       "m1-pair-001",
		Identity: Identity{Owner: "teradata-labs", Repo: "loom", PR: 411},
		Source:   Source{HeadRef: "refs/pull/411/head", BaseRef: "main"},
		Runtime:  Runtime{Name: "loom-isolated", Version: "1.4.0"},
		Models:   []Model{{Provider: "anthropic", Name: "claude-sonnet-5-5", Temperature: &temp}},
		Budget:   Budget{MaxWallSeconds: 3600, MaxToolCalls: 200, MaxModelCalls: 40, MaxCostUSD: 5},
	}
}

func TestProfileValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(p *Profile)
		wantErr string
	}{
		{name: "valid", mutate: func(p *Profile) {}},
		{name: "empty id", mutate: func(p *Profile) { p.ID = "" }, wantErr: "id"},
		{name: "uppercase id", mutate: func(p *Profile) { p.ID = "M1-Pair" }, wantErr: "id"},
		{name: "absent owner", mutate: func(p *Profile) { p.Identity.Owner = "" }, wantErr: "identity.owner"},
		{name: "owner with slash", mutate: func(p *Profile) { p.Identity.Owner = "a/b" }, wantErr: "identity.owner"},
		{name: "absent repo", mutate: func(p *Profile) { p.Identity.Repo = "" }, wantErr: "identity.repo"},
		{name: "zero pr", mutate: func(p *Profile) { p.Identity.PR = 0 }, wantErr: "identity.pr"},
		{name: "absent head ref", mutate: func(p *Profile) { p.Source.HeadRef = "" }, wantErr: "head_ref"},
		{name: "ref looks like a flag", mutate: func(p *Profile) { p.Source.BaseRef = "--output=x" }, wantErr: "base_ref"},
		{name: "ref with traversal", mutate: func(p *Profile) { p.Source.HeadRef = "a/../b" }, wantErr: "head_ref"},
		{name: "runtime without version or blocker", mutate: func(p *Profile) { p.Runtime.Version = "" }, wantErr: "runtime.version"},
		{name: "no models", mutate: func(p *Profile) { p.Models = nil }, wantErr: "models"},
		{name: "model without name", mutate: func(p *Profile) { p.Models[0].Name = "" }, wantErr: "models[0]"},
		{name: "temperature out of range", mutate: func(p *Profile) { v := 3.0; p.Models[0].Temperature = &v }, wantErr: "temperature"},
		{name: "absent wall budget", mutate: func(p *Profile) { p.Budget.MaxWallSeconds = 0 }, wantErr: "max_wall_seconds"},
		{name: "absent tool budget", mutate: func(p *Profile) { p.Budget.MaxToolCalls = 0 }, wantErr: "max_tool_calls"},
		{name: "absent model budget", mutate: func(p *Profile) { p.Budget.MaxModelCalls = 0 }, wantErr: "max_model_calls"},
		{name: "absent cost budget", mutate: func(p *Profile) { p.Budget.MaxCostUSD = 0 }, wantErr: "max_cost_usd"},
		{name: "NaN cost budget", mutate: func(p *Profile) { p.Budget.MaxCostUSD = math.NaN() }, wantErr: "max_cost_usd"},
		{name: "infinite cost budget", mutate: func(p *Profile) { p.Budget.MaxCostUSD = math.Inf(1) }, wantErr: "max_cost_usd"},
		{
			name: "blocker replaces version",
			mutate: func(p *Profile) {
				p.Runtime.Version = ""
				p.Runtime.Blocker = "no verified runtime on the isolated host"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validProfile()
			tt.mutate(&p)
			err := p.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRunnableFollowsBlocker(t *testing.T) {
	p := validProfile()
	if !p.Runnable() {
		t.Fatal("profile without blocker should be runnable")
	}
	p.Runtime.Blocker = "missing verified path"
	if p.Runnable() {
		t.Fatal("profile with blocker must not be runnable")
	}
}

func TestLoadRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	dir := t.TempDir()
	good := `{"id":"m1-pair-001","identity":{"owner":"o","repo":"r","pr":1},
"source":{"head_ref":"h","base_ref":"main"},"runtime":{"name":"x","version":"1"},
"models":[{"provider":"p","name":"m"}],
"budget":{"max_wall_seconds":1,"max_tool_calls":1,"max_model_calls":1,"max_cost_usd":1}}`

	cases := map[string]struct {
		body    string
		wantErr string
	}{
		"valid":          {body: good},
		"unknown field":  {body: strings.Replace(good, `"id":"m1-pair-001"`, `"id":"m1-pair-001","extra":true`, 1), wantErr: "unknown field"},
		"trailing data":  {body: good + ` {}`, wantErr: "trailing data"},
		"missing budget": {body: strings.Replace(good, `"budget":{"max_wall_seconds":1,"max_tool_calls":1,"max_model_calls":1,"max_cost_usd":1}`, `"budget":{}`, 1), wantErr: "budget"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}
