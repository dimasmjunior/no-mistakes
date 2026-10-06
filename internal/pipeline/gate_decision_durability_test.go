package pipeline

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The echo is a promise: a response must not be told what it recorded unless
// that decision is durable. With decision writes failing, the response is
// refused, the gate stays parked and nothing is dispatched; once writes work
// again the same response is accepted against that still-parked gate.
func TestExecutor_FixResponseRefusedWhenTheDecisionWriteFails(t *testing.T) {
	contexts := make(chan *StepContext, 4)
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			select {
			case contexts <- sctx:
			default:
			}
			return &StepOutcome{NeedsApproval: true, Findings: gateFindingsThree}, nil
		},
	}
	database, p, run, repo := setupTest(t)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	<-contexts // the parking round

	// A second connection to the same file faults only decision writes: history
	// reads and the step lifecycle keep working, which is what isolates the
	// write from everything else the response path does.
	raw, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER deny_decision BEFORE UPDATE OF selected_finding_ids ON step_rounds BEGIN SELECT RAISE(FAIL, 'injected decision write failure'); END;`); err != nil {
		t.Fatal(err)
	}

	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, []string{"R2", "R3"}, nil, nil, ""); err == nil {
		t.Fatal("a response whose decision cannot be recorded must be refused, never echoed")
	} else if !strings.Contains(err.Error(), "record") {
		t.Fatalf("err = %v, want the failed write named", err)
	}

	// Nothing was dispatched: no further round reached the step.
	select {
	case <-contexts:
		t.Fatal("a fix round ran although no decision was recorded")
	default:
	}

	// The gate is still parked: with writes working, the same response is
	// accepted against it.
	if _, err := raw.Exec(`DROP TRIGGER deny_decision`); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, []string{"R2", "R3"}, nil, nil, ""); err != nil {
		t.Fatalf("the still-parked gate refused the corrected response: %v", err)
	}

	approvalDeadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
			break
		} else if time.Now().After(approvalDeadline) {
			t.Fatalf("gate never accepted approval: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitExecutorDone(t, done)
}
