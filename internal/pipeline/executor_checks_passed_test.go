package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// releasingCIStep stands in for the CI step's terminal green verdict. It
// records exactly the durable state that step leaves behind when it releases a
// run - CI readiness and the open PR it observed - and returns a plain success
// outcome, which is what makes the CI step the last step of the run.
type releasingCIStep struct{}

func (releasingCIStep) Name() types.StepName { return types.StepCI }

func (releasingCIStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if err := sctx.DB.SetRunCIReadyWithReason(sctx.Run.ID, true, false); err != nil {
		return nil, err
	}
	if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "open"); err != nil {
		return nil, err
	}
	if sctx.CIReadinessChanged != nil {
		sctx.CIReadinessChanged(true, false)
	}
	return &StepOutcome{}, nil
}

// ciStepSnapshot is the recorded CI step state a terminal run must keep: its
// status and the wall-clock duration the executor measured for it.
type ciStepState struct {
	status     string
	durationMS int64
}

func ciStepSnapshot(t *testing.T, database *db.DB, runID string) ciStepState {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName != types.StepCI {
			continue
		}
		snapshot := ciStepState{status: string(step.Status)}
		if step.DurationMS != nil {
			snapshot.durationMS = *step.DurationMS
		}
		return snapshot
	}
	t.Fatalf("run %s has no CI step result", runID)
	return ciStepState{}
}

// TestExecutor_GreenCIRunRecordsChecksPassedAndReleases is the run-level half
// of the CI step's default verdict: a run whose checks came back green while
// its PR was still open finishes as checks_passed, not as an ordinary
// completion, so a caller can tell "checks passed, waiting on a human merge
// decision" apart from "the PR merged or closed" - and the merge is not this
// run's to observe.
//
// The second half proves the recorded outcome is stable and the duration is
// CI-only: the merge is observed afterwards (recorded as PR truth), and
// neither the run's status nor the CI step's status and duration move.
func TestExecutor_GreenCIRunRecordsChecksPassedAndReleases(t *testing.T) {
	database, p, run, repo := setupTest(t)
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{releasingCIStep{}}, events.handler)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunChecksPassed {
		t.Fatalf("run status = %s, want %s", got.Status, types.RunChecksPassed)
	}
	completed := events.findRunEvent(ipc.EventRunCompleted)
	if completed == nil || completed.Status == nil || *completed.Status != string(types.RunChecksPassed) {
		t.Fatalf("terminal event = %+v, want a checks_passed run_completed", completed)
	}

	before := ciStepSnapshot(t, database, run.ID)
	if before.status != string(types.StepStatusCompleted) {
		t.Fatalf("CI step status = %s, want completed", before.status)
	}

	// A merge observed afterwards is PR truth, and must not rewrite what this
	// run recorded: its outcome and the recorded CI duration stay put.
	if err := database.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	if repaired, err := database.ReconcileTerminalPRRuns(); err != nil {
		t.Fatalf("ReconcileTerminalPRRuns() error = %v", err)
	} else if repaired != 0 {
		t.Fatalf("reconciled runs = %d, want none: a checks_passed run is already terminal", repaired)
	}

	after, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != types.RunChecksPassed {
		t.Fatalf("run status after the merge observation = %s, want %s", after.Status, types.RunChecksPassed)
	}
	if after.PRState == nil || *after.PRState != "merged" {
		t.Fatalf("PR state = %v, want the merge recorded as PR truth", after.PRState)
	}
	afterStep := ciStepSnapshot(t, database, run.ID)
	if afterStep.status != before.status || afterStep.durationMS != before.durationMS {
		t.Fatalf("CI step after the merge observation = %+v, want it unchanged at %+v", afterStep, before)
	}
}

// TestExecutor_MergedCIRunStillRecordsCompleted keeps the other outcome
// distinct: a run that ended because the PR merged records an ordinary
// completed run, whose agent-facing outcome is "passed", not "checks-passed".
func TestExecutor_MergedCIRunStillRecordsCompleted(t *testing.T) {
	database, p, run, repo := setupTest(t)
	exec := NewExecutor(database, p, nil, nil, []Step{mergedCIStep{}}, nil)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want %s for a merged PR", got.Status, types.RunCompleted)
	}
}

// mergedCIStep is the CI step's other terminal shape: the PR merged, so the run
// completes rather than releasing at a green head.
type mergedCIStep struct{ releasingCIStep }

func (mergedCIStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if err := sctx.DB.SetRunCIReadyWithReason(sctx.Run.ID, true, false); err != nil {
		return nil, err
	}
	if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "merged"); err != nil {
		return nil, err
	}
	return &StepOutcome{}, nil
}
