package cmd

import (
	"fmt"
	"testing"
)

// fakeExitCoder satisfies interface{ ExitCode() int } like os/exec.ExitError
// does, but is deliberately not the internal exitError type — it must NOT be
// matched by exitCodeAndMessage's errors.As check (#217 review comment: a
// failing subprocess must not escape the documented 0/1/2 contract).
type fakeExitCoder struct{ code int }

func (e fakeExitCoder) Error() string { return "fake exit coder" }
func (e fakeExitCoder) ExitCode() int { return e.code }

func TestExitCodeAndMessage(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantCode     int
		wantPrintErr bool
	}{
		{
			name:         "plain error",
			err:          fmt.Errorf("boom"),
			wantCode:     exitCodeFailure,
			wantPrintErr: true,
		},
		{
			name:         "findings with no message stays silent",
			err:          exitError{code: exitCodeFindings},
			wantCode:     exitCodeFindings,
			wantPrintErr: false,
		},
		{
			name:         "findings with a message is printed",
			err:          exitError{code: exitCodeFindings, msg: "3 differences found"},
			wantCode:     exitCodeFindings,
			wantPrintErr: true,
		},
		{
			name:         "wrapped exitError is still matched",
			err:          fmt.Errorf("wrapping: %w", exitError{code: exitCodeFindings}),
			wantCode:     exitCodeFindings,
			wantPrintErr: false,
		},
		{
			name:         "unrelated ExitCode()-satisfying type does not leak its code",
			err:          fakeExitCoder{code: 7},
			wantCode:     exitCodeFailure,
			wantPrintErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, printErr := exitCodeAndMessage(tt.err)
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d", code, tt.wantCode)
			}
			if printErr != tt.wantPrintErr {
				t.Errorf("printErr = %v, want %v", printErr, tt.wantPrintErr)
			}
		})
	}
}
