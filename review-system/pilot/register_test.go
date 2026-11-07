package pilot

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisterValidate(t *testing.T) {
	base := func() Row {
		return Row{RowID: "W1.1.2", Repo: "R", Branch: "claude/focused-fermat-7n0f7a", Worktree: "/work/x", Status: StatusActive}
	}
	tests := []struct {
		name    string
		rows    []Row
		wantErr string
	}{
		{name: "active row", rows: []Row{base()}},
		{name: "bad row id", rows: []Row{func() Row { r := base(); r.RowID = "W9.1.1"; return r }()}, wantErr: "row_id"},
		{name: "duplicate row", rows: []Row{base(), base()}, wantErr: "duplicate"},
		{name: "unknown repo", rows: []Row{func() Row { r := base(); r.Repo = "X"; return r }()}, wantErr: "repo"},
		{name: "invalid branch", rows: []Row{func() Row { r := base(); r.Branch = "a..b"; return r }()}, wantErr: "branch"},
		{name: "pr url not github", rows: []Row{func() Row { r := base(); r.PRURL = "https://example.com/pull/1"; return r }()}, wantErr: "pr_url"},
		{name: "active without worktree", rows: []Row{func() Row { r := base(); r.Worktree = ""; return r }()}, wantErr: "worktree"},
		{name: "blocked without blocker", rows: []Row{func() Row { r := base(); r.Status = StatusBlocked; r.Worktree = ""; return r }()}, wantErr: "blocker"},
		{name: "done without evidence", rows: []Row{func() Row { r := base(); r.Status = StatusDone; return r }()}, wantErr: "evidence"},
		{name: "unknown status", rows: []Row{func() Row { r := base(); r.Status = "merged"; return r }()}, wantErr: "unknown status"},
		{name: "planned row needs nothing else", rows: []Row{{RowID: "W1.1.7", Repo: "R", Branch: "pr-review-w1-1-7", Status: StatusPlanned}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Register{Rows: tt.rows}.Validate()
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

func TestRegisterSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "register.json")
	want := Register{Rows: []Row{
		{RowID: "W1.1.1", Repo: "R", Branch: "b1", Worktree: "/w", Status: StatusDone, Evidence: "profile_test.go", PRURL: "https://github.com/Ed-Jaramillo/loom/pull/2"},
		{RowID: "W1.1.3", Repo: "R", Branch: "b3", Status: StatusBlocked, Blocker: "G-RUN not recorded", DependsOn: []string{"W1.1.2"}},
	}}
	if err := want.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadRegister(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Rows) != len(want.Rows) || got.Rows[1].Blocker != want.Rows[1].Blocker || got.Rows[0].PRURL != want.Rows[0].PRURL {
		t.Fatalf("round trip mismatch: got %+v", got)
	}
}

func TestSaveRefusesInvalidRegister(t *testing.T) {
	path := filepath.Join(t.TempDir(), "register.json")
	bad := Register{Rows: []Row{{RowID: "W1.1.1", Repo: "R", Branch: "b", Status: StatusActive}}}
	if err := bad.Save(path); err == nil {
		t.Fatal("save accepted an active row without a worktree")
	}
}
