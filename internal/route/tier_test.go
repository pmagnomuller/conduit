package route

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		d        Dossier
		want     string
		evidence int
	}{
		{name: "empty dossier is standard", d: Dossier{}, want: TierStandard, evidence: 1},
		{name: "large thinking budget is heavy", d: Dossier{Step: StepUserTurn, NMessages: 3, ThinkingBudget: 16000}, want: TierHeavy, evidence: 1},
		{name: "small thinking budget alone stays standard", d: Dossier{Step: StepUserTurn, NMessages: 3, ThinkingBudget: 2048}, want: TierStandard, evidence: 1},
		{
			name: "escalating tool errors are heavy",
			d:    Dossier{Step: StepToolStep, NMessages: 3, ToolBatch: &ToolBatch{Errors: 2}},
			want: TierHeavy, evidence: 1,
		},
		{
			name: "one tool error alone stays standard",
			d:    Dossier{Step: StepToolStep, NMessages: 3, ToolBatch: &ToolBatch{Errors: 1}},
			want: TierStandard, evidence: 1,
		},
		{
			name: "heavy keyword alone stays standard",
			d:    Dossier{Step: StepUserTurn, Task: "why does the renderer drop frames", NMessages: 3},
			want: TierStandard, evidence: 1,
		},
		{
			name: "heavy keyword plus wide tool set is heavy",
			d:    Dossier{Step: StepUserTurn, Task: "refactor the parser", NMessages: 3, NTools: 12},
			want: TierHeavy, evidence: 2,
		},
		{
			name: "light keyword plus short task is light",
			d:    Dossier{Step: StepUserTurn, Task: "fix the typo", NMessages: 1},
			want: TierLight, evidence: 2,
		},
		{
			name: "short task alone is not light",
			d:    Dossier{Step: StepUserTurn, Task: "help", NMessages: 2},
			want: TierStandard, evidence: 1,
		},
		{
			name: "fenced long task is not short",
			d:    Dossier{Step: StepUserTurn, Task: "run\n```" + string(make([]byte, 0)) + "\n", NMessages: 1},
			want: TierStandard, evidence: 0,
		},
		{
			name: "many messages defeat the short-task signal",
			d:    Dossier{Step: StepUserTurn, Task: "fix the typo", NMessages: 3},
			want: TierStandard, evidence: 1,
		},
		{
			name: "single navigation tool step is light with the keyword",
			d:    Dossier{Step: StepToolStep, Task: "bump", NMessages: 1, Tool: "Read"},
			want: TierLight, evidence: 3,
		},
		{
			name: "single navigation tool alone stays standard",
			d:    Dossier{Step: StepToolStep, NMessages: 3, Tool: "Grep"},
			want: TierStandard, evidence: 1,
		},
		{
			name: "a thinking budget blocks light",
			d:    Dossier{Step: StepToolStep, Task: "bump", NMessages: 1, Tool: "Read", ThinkingBudget: 100},
			want: TierStandard, evidence: 4,
		},
		{
			name: "step other is standard unconditionally",
			d:    Dossier{Step: StepOther, ThinkingBudget: 16000, Task: "why does this deadlock happen"},
			want: TierStandard, evidence: 3,
		},
	}
	for _, tc := range cases {
		got, evidence, reasons := classify(tc.d)
		if got != tc.want || evidence != tc.evidence {
			t.Errorf("%s: classify=%q evidence=%d want %q,%d (reasons=%v)", tc.name, got, evidence, tc.want, tc.evidence, reasons)
		}
		if evidence != len(reasons) {
			t.Errorf("%s: evidence %d != %d reasons", tc.name, evidence, len(reasons))
		}
	}
}

// classify must be pure: the same dossier always yields the same answer,
// whatever the map iteration order of the day. Run with -count=10 to prove
// the stability across hash seeds.
func TestClassifyIsPure(t *testing.T) {
	dossiers := []Dossier{
		{},
		{Step: StepUserTurn, ThinkingBudget: 16000},
		{Step: StepToolStep, ToolBatch: &ToolBatch{Errors: 3}, Tool: "Bash"},
		{Step: StepUserTurn, Task: "fix the typo", NMessages: 1, NTools: 13},
		{Step: StepOther, Task: "design the migration"},
	}
	for _, d := range dossiers {
		want, wantEv, wantReasons := classify(d)
		for i := 0; i < 10; i++ {
			got, ev, reasons := classify(d)
			if got != want || ev != wantEv || len(reasons) != len(wantReasons) {
				t.Fatalf("classify unstable for %+v: %q,%d vs %q,%d", d, got, ev, want, wantEv)
			}
			for j := range reasons {
				if reasons[j] != wantReasons[j] {
					t.Fatalf("reasons unstable for %+v: %v vs %v", d, reasons, wantReasons)
				}
			}
		}
	}
}

func tieredCands() []Candidate {
	return []Candidate{
		{Provider: "anthropic", Model: "claude-opus-5", Tier: TierHeavy},
		{Provider: "anthropic", Model: "claude-sonnet-5", Tier: TierStandard},
		{Provider: "glm", Model: "glm-5.3-flash", Tier: TierLight},
	}
}

func keys(cands []Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Key())
	}
	return out
}

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBand(t *testing.T) {
	all := tieredCands()
	cases := []struct {
		name      string
		cands     []Candidate
		tier      string
		wantKeys  []string
		effective string
		widened   bool
	}{
		{
			name: "own band present", cands: all, tier: TierHeavy,
			wantKeys: []string{"anthropic/claude-opus-5"}, effective: TierHeavy, widened: false,
		},
		{
			name: "heavy widens to standard", cands: all[1:], tier: TierHeavy,
			wantKeys: []string{"anthropic/claude-sonnet-5"}, effective: TierStandard, widened: true,
		},
		{
			name: "heavy widens down to light", cands: all[2:], tier: TierHeavy,
			wantKeys: []string{"glm/glm-5.3-flash"}, effective: TierLight, widened: true,
		},
		{
			name: "light widens up to standard", cands: all[:2], tier: TierLight,
			wantKeys: []string{"anthropic/claude-sonnet-5"}, effective: TierStandard, widened: true,
		},
		{
			name: "light never widens to heavy",
			cands: []Candidate{
				{Provider: "anthropic", Model: "claude-opus-5", Tier: TierHeavy},
				{Provider: "anthropic", Model: "claude-sonnet-5", Tier: TierStandard},
			}, tier: TierLight,
			wantKeys: []string{"anthropic/claude-sonnet-5"}, effective: TierStandard, widened: true,
		},
		{
			name: "standard prefers heavy over light",
			cands: []Candidate{
				{Provider: "anthropic", Model: "claude-opus-5", Tier: TierHeavy},
				{Provider: "glm", Model: "glm-5.3-flash", Tier: TierLight},
			}, tier: TierStandard,
			wantKeys: []string{"anthropic/claude-opus-5"}, effective: TierHeavy, widened: true,
		},
		{
			name: "standard falls to light when heavy is gone", cands: all[2:], tier: TierStandard,
			wantKeys: []string{"glm/glm-5.3-flash"}, effective: TierLight, widened: true,
		},
		{
			name: "blank tiers match no band", cands: []Candidate{
				{Provider: "anthropic", Model: "claude-opus-5"},
				{Provider: "glm", Model: "glm-5.3-flash"},
			}, tier: TierStandard,
			wantKeys: nil, effective: TierStandard, widened: false,
		},
	}
	for _, tc := range cases {
		out, effective, widened := band(tc.cands, tc.tier)
		if !sameKeys(keys(out), tc.wantKeys) {
			t.Errorf("%s: band keys=%v want %v", tc.name, keys(out), tc.wantKeys)
		}
		if effective != tc.effective || widened != tc.widened {
			t.Errorf("%s: effective=%q widened=%v want %q,%v", tc.name, effective, widened, tc.effective, tc.widened)
		}
	}
}

// Widening can only narrow the caller's slice: every member of the result
// must be a member of the input, and the input order must survive.
func TestBandNeverInventsOrReorders(t *testing.T) {
	cands := []Candidate{
		{Provider: "glm", Model: "glm-5.3-flash", Tier: TierLight},
		{Provider: "deepseek", Model: "deepseek-v4-flash", Tier: TierLight},
		{Provider: "anthropic", Model: "claude-opus-5", Tier: TierHeavy},
	}
	for _, tier := range []string{TierLight, TierStandard, TierHeavy} {
		out, _, _ := band(cands, tier)
		for _, c := range out {
			found := false
			for _, in := range cands {
				if in.Key() == c.Key() {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("tier %s: band invented %s", tier, c.Key())
			}
		}
	}
	out, _, _ := band(cands, TierLight)
	if !sameKeys(keys(out), []string{"glm/glm-5.3-flash", "deepseek/deepseek-v4-flash"}) {
		t.Fatalf("band reordered its input: %v", keys(out))
	}
}
