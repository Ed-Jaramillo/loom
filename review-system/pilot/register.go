package pilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Status is the state of one delivery row in the session register.
type Status string

const (
	StatusPlanned Status = "planned"
	StatusActive  Status = "active"
	StatusBlocked Status = "blocked"
	StatusDone    Status = "done"
)

var (
	rowIDPattern = regexp.MustCompile(`^W[1-7]\.[1-7]\.[1-7]$`)
	prURLPattern = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+/pull/[0-9]+$`)
)

// Row records one delivery row (for example W1.1.2) and its worktree. Each row
// names its repository: R (review-system) or L (Loom framework code).
type Row struct {
	RowID        string   `json:"row_id"`
	Repo         string   `json:"repo"`
	Branch       string   `json:"branch"`
	Worktree     string   `json:"worktree,omitempty"`
	AppSessionID string   `json:"app_session_id,omitempty"`
	PRURL        string   `json:"pr_url,omitempty"`
	Status       Status   `json:"status"`
	DependsOn    []string `json:"depends_on,omitempty"`
	Blocker      string   `json:"blocker,omitempty"`
	Evidence     string   `json:"evidence,omitempty"`
}

// Register is the session register for delivery rows.
type Register struct {
	Rows []Row `json:"rows"`
}

// Validate checks row IDs, status rules and PR URL shape.
func (r Register) Validate() error {
	var errs []error
	seen := map[string]bool{}
	for i, row := range r.Rows {
		prefix := fmt.Sprintf("rows[%d] (%s)", i, row.RowID)
		if !rowIDPattern.MatchString(row.RowID) {
			errs = append(errs, fmt.Errorf("%s: row_id must match %s", prefix, rowIDPattern))
		}
		if seen[row.RowID] {
			errs = append(errs, fmt.Errorf("%s: duplicate row_id", prefix))
		}
		seen[row.RowID] = true
		if row.Repo != "R" && row.Repo != "L" {
			errs = append(errs, fmt.Errorf("%s: repo must be R or L", prefix))
		}
		if !validRef(row.Branch) {
			errs = append(errs, fmt.Errorf("%s: branch %q is not a valid ref", prefix, row.Branch))
		}
		if row.PRURL != "" && !prURLPattern.MatchString(row.PRURL) {
			errs = append(errs, fmt.Errorf("%s: pr_url must be a github.com pull request URL", prefix))
		}
		switch row.Status {
		case StatusPlanned:
		case StatusActive:
			if row.Worktree == "" {
				errs = append(errs, fmt.Errorf("%s: active rows need a worktree", prefix))
			}
		case StatusBlocked:
			if row.Blocker == "" {
				errs = append(errs, fmt.Errorf("%s: blocked rows need a blocker", prefix))
			}
		case StatusDone:
			if row.Evidence == "" {
				errs = append(errs, fmt.Errorf("%s: done rows need evidence", prefix))
			}
		default:
			errs = append(errs, fmt.Errorf("%s: unknown status %q", prefix, row.Status))
		}
	}
	return errors.Join(errs...)
}

// Save validates the register, then replaces the file atomically.
func (r Register) Save(path string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode register: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".register-*.json")
	if err != nil {
		return fmt.Errorf("create temp register: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp register: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp register: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}

// LoadRegister reads and validates a register file.
func LoadRegister(path string) (Register, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Register{}, fmt.Errorf("read register: %w", err)
	}
	var r Register
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Register{}, fmt.Errorf("decode register: %w", err)
	}
	if err := r.Validate(); err != nil {
		return Register{}, err
	}
	return r, nil
}
