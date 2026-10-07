package pipeline

import (
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// FindingDecisionKey identifies a finding by what it describes rather than by
// the ID it happened to carry in one round. A gate hands an earlier finding to
// a later round under a fresh ID, and a later review can reuse an old ID for a
// different finding, so an ID alone cannot say whether two appearances are the
// same decision. findingKey drops the fields a round owns (ID, action, source,
// instructions) and keeps the finding's substance.
func FindingDecisionKey(item types.Finding) string {
	encoded, _ := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{findingKey(item)}})
	return encoded
}

// ChosenFindingKeys returns the decision keys of the findings this round's
// human selection chose to fix. An auto-fix selection is the pipeline's own
// choice and decides nothing, and an approve, skip, or abort selected nothing,
// so both return an empty set.
func ChosenFindingKeys(round *db.StepRound) map[string]bool {
	keys := make(map[string]bool)
	if !humanDecidedRound(round) {
		return keys
	}
	for _, item := range selectedFindingIdentities([]*db.StepRound{round}) {
		keys[FindingDecisionKey(item)] = true
	}
	return keys
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
