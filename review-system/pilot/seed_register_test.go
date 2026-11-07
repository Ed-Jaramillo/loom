package pilot

import "testing"

// TestSeedRegisterMatchesStageOne loads the checked-in register through the
// same schema the tool uses, and checks that stage 1.1 rows are present once.
func TestSeedRegisterMatchesStageOne(t *testing.T) {
	reg, err := LoadRegister("session-register.json")
	if err != nil {
		t.Fatalf("load seed register: %v", err)
	}
	want := []string{"W1.1.1", "W1.1.2", "W1.1.3", "W1.1.4", "W1.1.5", "W1.1.6", "W1.1.7"}
	if len(reg.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(reg.Rows), len(want))
	}
	for i, row := range reg.Rows {
		if row.RowID != want[i] {
			t.Errorf("row %d = %s, want %s", i, row.RowID, want[i])
		}
		if row.PRURL != "" {
			t.Errorf("%s has a PR URL, but PRs are deferred for this stage", row.RowID)
		}
	}
}
