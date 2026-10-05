package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// gateFindingsThree is the shape of a fix-review gate: a carried ask-user
// finding the human already decided on, and two auto-fix findings.
const gateFindingsThree = `{"findings":[` +
	`{"id":"R1","severity":"error","file":"a.go","description":"first","action":"ask-user"},` +
	`{"id":"R2","severity":"error","file":"a.go","description":"second","action":"auto-fix"},` +
	`{"id":"R3","severity":"warning","file":"b.go","description":"third","action":"auto-fix"}],` +
	`"summary":"3 findings"}`

func selectionRound(t *testing.T, source, findings string, selected ...string) *db.StepRound {
	t.Helper()
	selectedJSON, err := json.Marshal(selected)
	if err != nil {
		t.Fatal(err)
	}
	selectedText := string(selectedJSON)
	return &db.StepRound{
		FindingsJSON:       &findings,
		SelectedFindingIDs: &selectedText,
		SelectionSource:    &source,
	}
}

func userDecisionRound(t *testing.T, findings string, selected ...string) *db.StepRound {
	t.Helper()
	return selectionRound(t, db.RoundSelectionSourceUser, findings, selected...)
}

func TestSplitFixResponse_RefusesAFindingTheResponseNeverAccountsFor(t *testing.T) {
	_, err := splitFixResponse(gateFindingsThree, nil, []string{"R1"}, nil)

	var refusal *RespondRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a RespondRefusal", err)
	}
	if refusal.Message != "R2,R3 not addressed: list them in --findings or --ignore" {
		t.Fatalf("message = %q, want the unaccounted ids named", refusal.Message)
	}
	if strings.Join(refusal.Missing, ",") != "R2,R3" {
		t.Fatalf("missing = %v, want [R2 R3]", refusal.Missing)
	}
	if refusal.Help == "" {
		t.Fatal("refusal carries no help line")
	}
	if !strings.Contains(refusal.Help, "--ignore") {
		t.Fatalf("help does not say how to decline: %q", refusal.Help)
	}
}

func TestSplitFixResponse_RefusesUnknownAndDoubleListedIDs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		findings []string
		ignore   []string
		want     string
	}{
		{name: "unknown in findings", findings: []string{"R9"}, want: "R9 not shown at this gate"},
		{name: "unknown in ignore", ignore: []string{"R9"}, want: "R9 not shown at this gate"},
		{name: "id in both lists", findings: []string{"R1", "R2", "R3"}, ignore: []string{"R2"}, want: "R2 listed in both --findings and --ignore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := splitFixResponse(gateFindingsThree, nil, tc.findings, tc.ignore)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestSplitFixResponse_KeepsAnEarlierDecisionOnOmission(t *testing.T) {
	rounds := []*db.StepRound{userDecisionRound(t, gateFindingsThree, "R1")}

	got, err := splitFixResponse(gateFindingsThree, rounds, []string{"R2"}, []string{"R3"})
	if err != nil {
		t.Fatalf("omitting a finding an earlier round decided must be allowed: %v", err)
	}
	if strings.Join(got.Fixed, ",") != "R2" || strings.Join(got.Ignored, ",") != "R3" || strings.Join(got.Kept, ",") != "R1" {
		t.Fatalf("dispositions = %+v, want fixed=[R2] ignored=[R3] kept=[R1]", got)
	}
}

// A gate response cannot reverse an applied fix: --ignore naming a finding an
// earlier user round of the same step already chose to fix is refused, and the
// refusal names those findings so a caller can correct itself by omitting them.
func TestSplitFixResponse_RefusesDecliningAFindingChosenToFixEarlier(t *testing.T) {
	rounds := []*db.StepRound{userDecisionRound(t, gateFindingsThree, "R1")}

	_, err := splitFixResponse(gateFindingsThree, rounds, []string{"R2"}, []string{"R1", "R3"})

	var refusal *RespondRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a RespondRefusal", err)
	}
	if !strings.Contains(refusal.Message, "R1") || !strings.Contains(refusal.Message, "already chosen to fix") {
		t.Fatalf("message = %q, want the already-fixed id named", refusal.Message)
	}
	if strings.Join(refusal.DeclinedEarlierFix, ",") != "R1" {
		t.Fatalf("declinedEarlierFix = %v, want [R1]", refusal.DeclinedEarlierFix)
	}
	if !strings.Contains(refusal.Help, "out of scope for a gate response") {
		t.Fatalf("help = %q, want the out-of-scope sentence", refusal.Help)
	}
	// The declaration is refused as a whole: R3 alone would have been fine, but
	// nothing is recorded from a refused response.
	if len(refusal.Missing) != 0 {
		t.Fatalf("missing = %v, want none: every finding was accounted for", refusal.Missing)
	}
}

func TestSplitFixResponse_AnAutoFixSelectionDecidesNothing(t *testing.T) {
	rounds := []*db.StepRound{selectionRound(t, db.RoundSelectionSourceAutoFix, gateFindingsThree, "R2")}

	_, err := splitFixResponse(gateFindingsThree, rounds, []string{"R1"}, nil)
	var refusal *RespondRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal: an auto-fix complement is not a human decision", err)
	}
	if strings.Join(refusal.Missing, ",") != "R2,R3" {
		t.Fatalf("missing = %v, want [R2 R3]", refusal.Missing)
	}
}

func TestSplitFixResponse_AnApprovalDecidesEveryFindingItShowed(t *testing.T) {
	shown := `{"findings":[{"id":"R2","severity":"error","file":"a.go","description":"second","action":"auto-fix"}]}`
	rounds := []*db.StepRound{selectionRound(t, db.RoundSelectionSourceUserDeclined, shown, nil...)}

	got, err := splitFixResponse(gateFindingsThree, rounds, []string{"R1"}, []string{"R3"})
	if err != nil {
		t.Fatalf("a finding an earlier approve declined must not need restating: %v", err)
	}
	if strings.Join(got.Kept, ",") != "R2" {
		t.Fatalf("kept = %v, want [R2]", got.Kept)
	}
}

// The shape `--yes` and the auto-driver send: every finding the gate shows,
// with no declines.
func TestSplitFixResponse_EveryFindingSelectedWithNoDeclinesPasses(t *testing.T) {
	got, err := splitFixResponse(gateFindingsThree, nil, []string{"R1", "R2", "R3"}, nil)
	if err != nil {
		t.Fatalf("selecting every finding must never be refused: %v", err)
	}
	if strings.Join(got.Fixed, ",") != "R1,R2,R3" || len(got.Ignored) != 0 || len(got.Kept) != 0 {
		t.Fatalf("dispositions = %+v, want every finding fixed and nothing else", got)
	}
}

func TestSplitFixResponse_DispositionsAreInGateOrder(t *testing.T) {
	got, err := splitFixResponse(gateFindingsThree, nil, []string{"R3", "R1"}, []string{"R2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Fixed, ",") != "R1,R3" {
		t.Fatalf("fixed = %v, want gate order", got.Fixed)
	}
	if strings.Join(got.Ignored, ",") != "R2" || len(got.Kept) != 0 {
		t.Fatalf("dispositions = %+v, want ignored=[R2] and nothing kept", got)
	}
}

// A findings payload this cannot parse leaves nothing to account for, matching
// how the rest of the pipeline degrades on a malformed payload instead of
// blocking the gate forever.
func TestSplitFixResponse_UnparseableGateLeavesNothingToAccountFor(t *testing.T) {
	if _, err := splitFixResponse("{not json", nil, []string{"R1"}, nil); err != nil {
		t.Fatalf("err = %v, want the response accepted with nothing to account for", err)
	}
}

// respondFixStepFindings drives one review step through a gate that shows the
// three-finding payload on every round.
func respondFixStepFindings(t *testing.T) (*Executor, *db.DB, *db.Run, *db.Repo, <-chan error) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{NeedsApproval: true, Findings: gateFindingsThree}, nil
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	return exec, database, run, repo, done
}

// The refusal happens before the gate is resolved, so the response can be
// corrected and sent again.
func TestExecutor_FixResponseRefusalLeavesTheGateParked(t *testing.T) {
	exec, database, run, _, done := respondFixStepFindings(t)

	_, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, nil, nil, nil, "")
	var refusal *RespondRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if strings.Join(refusal.Missing, ",") != "R2,R3" {
		t.Fatalf("missing = %v, want [R2 R3]", refusal.Missing)
	}

	dispositions, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, []string{"R2", "R3"}, nil, nil, "")
	if err != nil {
		t.Fatalf("corrected response refused: %v", err)
	}
	if strings.Join(dispositions.Fixed, ",") != "R1" || strings.Join(dispositions.Ignored, ",") != "R2,R3" {
		t.Fatalf("dispositions = %+v, want fixed=[R1] ignored=[R2,R3]", dispositions)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

// An omission of a finding an earlier round of the same step already decided is
// kept, never re-declined, and a later --ignore naming an earlier fix is
// refused: the gate stays parked and the caller corrects the response.
func TestExecutor_FixResponseKeepsEarlierDecisionsAndRefusesTheirReversal(t *testing.T) {
	exec, database, run, _, done := respondFixStepFindings(t)
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps = %+v, %v", steps, err)
	}

	// The decision is written by the executor's gate loop after it consumes
	// the response, and a response can arrive before the next park registers,
	// so both the send and the write are waited for rather than assumed.
	respond := func(selected, ignored []string) RespondDispositions {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			dispositions, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, selected, ignored, nil, nil, "")
			if err == nil {
				return dispositions
			}
			var refusal *RespondRefusal
			if errors.As(err, &refusal) {
				t.Fatalf("response refused: %v", err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("gate never accepted the response: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForDecision := func(wantRound int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			rounds, err := database.GetRoundsByStep(steps[0].ID)
			if err == nil && len(rounds) >= wantRound && rounds[wantRound-1].SelectionSource != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("round %d never recorded its decision", wantRound)
	}

	// Round 1: fix R1, decline R2 and R3.
	dispositions := respond([]string{"R1"}, []string{"R2", "R3"})
	if strings.Join(dispositions.Fixed, ",") != "R1" || strings.Join(dispositions.Ignored, ",") != "R2,R3" || len(dispositions.Kept) != 0 {
		t.Fatalf("dispositions = %+v, want fixed=[R1] ignored=[R2,R3] kept=[]", dispositions)
	}
	waitForDecision(1)

	// Round 2: fix R2 and R3, omit R1 - which round 1 already decided - and
	// decline nothing new.
	dispositions = respond([]string{"R2", "R3"}, nil)
	if strings.Join(dispositions.Kept, ",") != "R1" {
		t.Fatalf("kept = %v, want [R1]", dispositions.Kept)
	}
	waitForDecision(2)

	// Round 3: trying to decline R1 is refused, and the gate stays parked. The
	// attempt races the re-park, so it retries until the gate is there.
	refusalDeadline := time.Now().Add(5 * time.Second)
	var refusal *RespondRefusal
	for {
		_, err = exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R2", "R3"}, []string{"R1"}, nil, nil, "")
		if errors.As(err, &refusal) {
			break
		}
		if err == nil {
			t.Fatal("declining a finding an earlier round chose to fix was accepted")
		}
		if !strings.Contains(err.Error(), "no step awaiting approval") {
			t.Fatalf("unexpected respond error: %v", err)
		}
		if time.Now().After(refusalDeadline) {
			t.Fatalf("gate never refused the reversal: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Join(refusal.DeclinedEarlierFix, ",") != "R1" {
		t.Fatalf("declinedEarlierFix = %v, want [R1]", refusal.DeclinedEarlierFix)
	}
	// The gate is still there: the corrected response (R1 omitted) is accepted.
	dispositions = respond([]string{"R2", "R3"}, nil)
	if strings.Join(dispositions.Kept, ",") != "R1" {
		t.Fatalf("kept = %v, want [R1]", dispositions.Kept)
	}
	waitForDecision(3)

	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) < 3 {
		t.Fatalf("rounds = %d, want at least the three answered rounds", len(rounds))
	}
	if got := selectedIDsOf(t, rounds[0]); strings.Join(got, ",") != "R1" {
		t.Fatalf("round 1 selected = %v, want [R1]: the decline set is its complement", got)
	}
	if got := selectedIDsOf(t, rounds[1]); strings.Join(got, ",") != "R2,R3" {
		t.Fatalf("round 2 selected = %v, want [R2,R3]", got)
	}

	// End the run: the fix round after the last response re-parks, possibly
	// after this point, so approve once the gate is there.
	approvalDeadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.Respond(types.StepReview, types.ActionApprove, nil)
		if err == nil {
			break
		}
		if time.Now().After(approvalDeadline) {
			t.Fatalf("gate never accepted approval: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitExecutorDone(t, done)
}

func selectedIDsOf(t *testing.T, round *db.StepRound) []string {
	t.Helper()
	return selectionIDsOf(t, round.SelectedFindingIDs)
}

// A recovered gate applies the same validation and recording as a live one: an
// unaccounted response is refused with the gate still parked, and a corrected
// response records its selection on the recovered round.
func TestExecutor_RecoveredGateValidatesAndRecordsTheDecision(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := gateFindingsThree
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, "1111111111111111111111111111111111111111", 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if !sctx.Fixing {
				return nil, errors.New("recovered gate must not rerun its completed review pass")
			}
			return &StepOutcome{FixSummary: "applied"}, nil
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Resume(context.Background(), run, repo, t.TempDir())
	}()

	// The recovered park is registered asynchronously, so the refusal is
	// asserted inside the retry loop that waits for it.
	deadline := time.Now().Add(5 * time.Second)
	refused := false
	for time.Now().Before(deadline) {
		_, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, nil, nil, nil, "")
		var refusal *RespondRefusal
		if errors.As(err, &refusal) {
			if strings.Join(refusal.Missing, ",") != "R2,R3" {
				t.Fatalf("missing = %v, want [R2 R3]", refusal.Missing)
			}
			refused = true
			break
		}
		if err != nil && !strings.Contains(err.Error(), "no step awaiting approval") {
			t.Fatalf("unexpected respond error: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !refused {
		t.Fatal("recovered gate never refused the unaccounted response")
	}

	dispositions := RespondDispositions{}
	sent := false
	for time.Now().Before(deadline) {
		dispositions, err = exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, []string{"R2", "R3"}, nil, nil, "")
		if err == nil {
			sent = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sent {
		t.Fatalf("recovered gate never accepted the corrected response: %v", err)
	}
	if strings.Join(dispositions.Ignored, ",") != "R2,R3" {
		t.Fatalf("ignored = %v, want [R2,R3]", dispositions.Ignored)
	}

	recorded := false
	for time.Now().Before(deadline) {
		rounds, err := database.GetRoundsByStep(stepResult.ID)
		if err == nil && len(rounds) > 0 && rounds[0].SelectionSource != nil {
			if got := selectedIDsOf(t, rounds[0]); strings.Join(got, ",") != "R1" {
				t.Fatalf("recovered round selected = %v, want [R1]", got)
			}
			recorded = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !recorded {
		t.Fatal("recovered gate never recorded the decision")
	}

	approvalSent := false
	for time.Now().Before(deadline) {
		if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
			approvalSent = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !approvalSent {
		t.Fatal("recovered gate never accepted approval")
	}
	waitExecutorDone(t, done)
}

func selectionIDsOf(t *testing.T, raw *string) []string {
	t.Helper()
	if raw == nil {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(*raw), &ids); err != nil {
		t.Fatalf("decode selection %q: %v", *raw, err)
	}
	return ids
}
