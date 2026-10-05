package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RespondDispositions is what a gate response actually recorded, reported back
// to the caller so a driver sees an inverted or surprising decision
// immediately instead of two rounds later, when a reviewer reports the
// contradiction.
//
// All three lists are in gate order, and each is empty when it recorded
// nothing.
type RespondDispositions struct {
	// Fixed names the findings the response selected to fix.
	Fixed []string
	// Ignored names the findings the response explicitly declined.
	Ignored []string
	// Kept names the findings the response omitted that an earlier round of
	// the same step had already decided, so it kept that decision instead of
	// declining them by omission.
	Kept []string
}

// RespondRefusal is a fix response the executor refused without touching the
// gate. The gate stays parked, so the caller can correct the response and
// send it again.
type RespondRefusal struct {
	// Message names what is wrong with the response, including the finding IDs
	// it failed to account for.
	Message string
	// Missing names the findings the response left unaccounted for, so a
	// structured surface can render them without parsing Message.
	Missing []string
	// DeclinedEarlierFix names the findings the response tried to decline that
	// an earlier round of the same step already chose to fix. Reverting an
	// applied fix is out of scope for a gate response.
	DeclinedEarlierFix []string
	// Help is the next action for the caller.
	Help string
}

func (r *RespondRefusal) Error() string {
	if r == nil {
		return ""
	}
	return r.Message
}

const fixResponseHelp = "List every finding the gate shows in --findings or --ignore; a finding an earlier round of this step already decided may be omitted to keep that decision."

// fixResponseRevertHelp is the refusal help for an --ignore that names a
// finding an earlier round of the same step already chose to fix.
const fixResponseRevertHelp = "Reverting an applied fix is out of scope for a gate response: leave those findings out of --ignore to keep the earlier decision, or list them in --findings to have the pipeline fix them again."

// splitFixResponse validates a fix response against the gate that is parked
// and splits it into the dispositions it will record.
//
// Declines are explicit: every finding the gate shows must appear in
// findingIDs or ignoreFindingIDs unless an earlier round of this step already
// decided it, and an ID in both lists or an ID the gate does not show is
// refused. The validation exists because a partial selection used to record
// its complement as declined, so an omitted finding - including one an earlier
// round had already fixed, which the round-history carry re-shows at a later
// gate - silently became a human decline and the next reviewer was told to
// undo work the same human had asked for: in a real run, round 2 recorded
// five findings a human had twice chosen to fix as declined, because the
// driving agent's response listed only the eight it meant to fix.
//
// A finding an earlier user round already decided keeps that decision when it
// is omitted (Kept), which is what makes an earlier fix sticky. Naming one of
// those findings in ignoreFindingIDs is refused when the earlier decision was
// to FIX it: a gate response cannot reverse an applied fix, so that reversal is
// out of scope here. A finding an earlier round declined may be restated as
// declined. "Earlier user round" means a round of the same step result whose
// selection came from a human - an auto-fix selection is the pipeline's own
// choice and decides nothing.
//
// The decline set itself is not stored: it is the complement of the selection
// (issue #790 decision B) minus the findings an earlier round chose to fix,
// derived on read by declinedFindingLines.
//
// The gate's findings payload failing to parse leaves nothing to account for,
// so the response passes: every other part of the pipeline degrades the same
// way rather than blocking a gate on a malformed payload.
func splitFixResponse(gateFindingsJSON string, rounds []*db.StepRound, findingIDs, ignoreFindingIDs []string) (RespondDispositions, error) {
	gate, gateParsed := parseGateFindingIDs(gateFindingsJSON)
	inGate := make(map[string]bool, len(gate))
	for _, id := range gate {
		inGate[id] = true
	}

	fixed := uniqueNonEmpty(findingIDs)
	ignored := uniqueNonEmpty(ignoreFindingIDs)

	// A payload this cannot decode shows no findings to account for and no
	// earlier decision to protect, so the response is accepted and its
	// explicit declines are recorded as sent. Refusing would strand the gate
	// behind approving or skipping a step whose findings nobody can read,
	// and nothing downstream can act on a payload this cannot parse anyway.
	if !gateParsed {
		return RespondDispositions{Fixed: fixed, Ignored: ignored}, nil
	}
	if unknown := idsOutside(fixed, inGate); len(unknown) > 0 {
		return RespondDispositions{}, &RespondRefusal{
			Message: fmt.Sprintf("%s not shown at this gate", strings.Join(unknown, ",")),
			Help:    fixResponseHelp,
		}
	}
	if unknown := idsOutside(ignored, inGate); len(unknown) > 0 {
		return RespondDispositions{}, &RespondRefusal{
			Message: fmt.Sprintf("%s not shown at this gate", strings.Join(unknown, ",")),
			Help:    fixResponseHelp,
		}
	}

	fixSet := make(map[string]bool, len(fixed))
	for _, id := range fixed {
		fixSet[id] = true
	}
	ignoreSet := make(map[string]bool, len(ignored))
	for _, id := range ignored {
		ignoreSet[id] = true
	}
	var both []string
	for _, id := range gate {
		if fixSet[id] && ignoreSet[id] {
			both = append(both, id)
		}
	}
	if len(both) > 0 {
		return RespondDispositions{}, &RespondRefusal{
			Message: fmt.Sprintf("%s listed in both --findings and --ignore", strings.Join(both, ",")),
			Help:    fixResponseHelp,
		}
	}

	chosenToFix := earlierChosenToFixIDs(rounds)
	var reversals []string
	for _, id := range gate {
		if ignoreSet[id] && chosenToFix[id] {
			reversals = append(reversals, id)
		}
	}
	if len(reversals) > 0 {
		return RespondDispositions{}, &RespondRefusal{
			Message:            fmt.Sprintf("%s already chosen to fix in an earlier round of this step", strings.Join(reversals, ",")),
			DeclinedEarlierFix: reversals,
			Help:               fixResponseRevertHelp,
		}
	}

	decided := earlierUserDecisionIDs(rounds)
	var unaccounted []string
	for _, id := range gate {
		if fixSet[id] || ignoreSet[id] || decided[id] {
			continue
		}
		unaccounted = append(unaccounted, id)
	}
	if len(unaccounted) > 0 {
		return RespondDispositions{}, &RespondRefusal{
			Message: fmt.Sprintf("%s not addressed: list them in --findings or --ignore", strings.Join(unaccounted, ",")),
			Missing: unaccounted,
			Help:    fixResponseHelp,
		}
	}

	dispositions := RespondDispositions{
		Fixed:   idsInPayloadOrder(fixed, gate),
		Ignored: idsInPayloadOrder(ignored, gate),
	}
	for _, id := range gate {
		if decided[id] && !fixSet[id] && !ignoreSet[id] {
			dispositions.Kept = append(dispositions.Kept, id)
		}
	}
	return dispositions, nil
}

// earlierUserDecisionIDs returns every finding ID a human decided in a round of
// this step result: the findings the round showed, which its recorded
// selection either chose to fix or declined. An approve, skip, or abort
// resolution decides every finding the round showed the same way.
//
// The auto-fix source is deliberately excluded: its complement is the findings
// the auto-fix filter left for a later gate, not a human decision, and the
// round-history rendering keeps that same distinction (`auto_fix_left_unselected`).
func earlierUserDecisionIDs(rounds []*db.StepRound) map[string]bool {
	decided := make(map[string]bool)
	for _, round := range rounds {
		if round == nil || round.FindingsJSON == nil {
			continue
		}
		if !humanDecidedRound(round) {
			continue
		}
		for _, id := range findingIDsInPayloadOrder(*round.FindingsJSON) {
			decided[id] = true
		}
	}
	return decided
}

// earlierChosenToFixIDs returns the finding IDs a human explicitly chose to fix
// in a round of this step result. A gate response may not decline one of them:
// reversal of an applied fix is out of scope for a gate response, so the
// caller must omit the finding to keep that decision instead.
func earlierChosenToFixIDs(rounds []*db.StepRound) map[string]bool {
	chosen := make(map[string]bool)
	for _, round := range rounds {
		if round == nil || !humanDecidedRound(round) || round.SelectedFindingIDs == nil {
			continue
		}
		var ids []string
		if err := json.Unmarshal([]byte(*round.SelectedFindingIDs), &ids); err != nil {
			continue
		}
		for _, id := range ids {
			if id != "" {
				chosen[id] = true
			}
		}
	}
	return chosen
}

// humanDecidedRound reports whether this round's selection came from a human:
// a user fix selection or an approve/skip/abort resolution. An auto-fix
// selection is the pipeline's own choice and decides nothing.
func humanDecidedRound(round *db.StepRound) bool {
	if round == nil || round.SelectionSource == nil {
		return false
	}
	switch *round.SelectionSource {
	case db.RoundSelectionSourceUser, db.RoundSelectionSourceUserDeclined:
		return true
	default:
		return false
	}
}

// findingIDsInPayloadOrder returns the IDs of the findings payload in the order
// the gate shows them, without duplicates, skipping findings that carry no ID
// (they cannot be selected or declined by ID).
func findingIDsInPayloadOrder(raw string) []string {
	ids, _ := parseGateFindingIDs(raw)
	return ids
}

// parseGateFindingIDs is findingIDsInPayloadOrder plus whether the payload
// could be decoded at all. An empty payload decodes to no findings; only a
// malformed one reports false, and the caller must then skip validation
// entirely rather than treat every ID as unknown.
func parseGateFindingIDs(raw string) ([]string, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, true
	}
	parsed, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil, false
	}
	ids := make([]string, 0, len(parsed.Items))
	seen := make(map[string]bool, len(parsed.Items))
	for _, item := range parsed.Items {
		id := item.ID
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, true
}

func uniqueNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func idsOutside(ids []string, inGate map[string]bool) []string {
	var out []string
	for _, id := range ids {
		if !inGate[id] {
			out = append(out, id)
		}
	}
	return out
}

// idsInPayloadOrder orders the given IDs the way the gate shows them.
func idsInPayloadOrder(ids []string, gate []string) []string {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	out := make([]string, 0, len(ids))
	for _, id := range gate {
		if set[id] {
			out = append(out, id)
		}
	}
	return out
}
