package qualify

import (
	"slices"
	"strings"
	"testing"

	"github.com/daniil4545/lead-qualifier/internal/config"
)

// testRules - the test catalog: own thresholds and prices so the test does not depend on data/example.
func testRules() Rules {
	return NewRules(config.Catalog{
		Tariffs: []config.Tariff{
			{Name: "Малый", Threshold: 1_000_000, Price: 40_000},
			{Name: "Средний", Threshold: 5_000_000, Price: 80_000},
		},
		Segments: []config.Segment{
			{Code: "retail", Name: "торговля", Threshold: 0},
			{Code: "clinic", Name: "клиники", Threshold: 3_000_000},
		},
		RefusalReasons: []config.RefusalReason{{Code: "gambling", Name: "азартные игры"}},
	})
}

func ptr[T any](value T) *T { return &value }

// leadFacts - a full set of facts with the niche verdict.
func leadFacts(rules Rules, revenue string, verdict NicheVerdict) Facts {
	facts := Merge(Facts{}, FactDelta{
		Service: ptr("сеть магазинов"),
		Revenue: ptr(revenue),
		Role:    ptr("директор"),
		IsOwner: ptr(true),
		City:    ptr("Томск"),
	})
	rules.ApplyVerdict(facts, verdict)
	return facts
}

func TestGate_NicheThenRevenue(t *testing.T) {
	t.Parallel()
	rules := testRules()
	retail := NicheVerdict{Direction: "retail"}
	clinic := NicheVerdict{Direction: "clinic"}
	offProfile := NicheVerdict{OffProfileReason: "gambling"}
	cases := []struct {
		name    string
		revenue string
		verdict NicheVerdict
		want    Gate
	}{
		{"tariff threshold is inclusive", "1000000", retail, GateOffer},
		{"threshold minus one is too early", "999999", retail, GateTooEarly},
		{"threshold plus one", "1000001", retail, GateOffer},
		{"segment threshold minus one", "2999999", clinic, GateTooEarly},
		{"segment threshold", "3000000", clinic, GateOffer},
		{"unclear niche uses min tariff threshold", "1000000", NicheVerdict{Direction: NicheUnclear}, GateOffer},
		{"unknown direction drops refusal reason", "1000000", NicheVerdict{Direction: "casino", OffProfileReason: "gambling"}, GateOffer},
		{"off profile with high revenue", "50000000", offProfile, GateOffProfile},
		{"off profile wins over low revenue", "100", offProfile, GateOffProfile},
		{"target and refusal niche together", "5000000", NicheVerdict{Direction: "retail", OffProfileReason: "gambling"}, GateMixedNiche},
		{"gate uses upper bound of range", "800000-1200000", retail, GateOffer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := rules.Gate(leadFacts(rules, tc.revenue, tc.verdict))
			if got != tc.want {
				t.Fatalf("Gate(revenue=%q, verdict=%+v) = %q, want %q", tc.revenue, tc.verdict, got, tc.want)
			}
		})
	}
}

func TestGate_AllowsMeeting(t *testing.T) {
	t.Parallel()
	want := map[Gate]bool{GateOffer: true, GateMixedNiche: true, GateTooEarly: false, GateOffProfile: false}
	for gate, allows := range want {
		if gate.AllowsMeeting() != allows {
			t.Errorf("%q.AllowsMeeting() = %v, want %v", gate, !allows, allows)
		}
	}
}

func TestTariffFor_LowerBoundInclusive(t *testing.T) {
	t.Parallel()
	rules := testRules()
	cases := []struct {
		revenue string
		want    string // tariff name; empty means no tariff
	}{
		{"999999", ""},
		{"1000000", "Малый"},
		{"4999999", "Малый"},
		{"5000000", "Средний"},
		{"800000-1200000", ""},
		{"3000000-6000000", "Малый"},
	}
	for _, tc := range cases {
		tariff, ok := rules.TariffFor(leadFacts(rules, tc.revenue, NicheVerdict{Direction: "retail"}))
		got := ""
		if ok {
			got = tariff.Name
		}
		if got != tc.want {
			t.Errorf("TariffFor(revenue=%q) = %q, want %q", tc.revenue, got, tc.want)
		}
	}
}

func TestNormalizeVerdict_UnknownDirectionIsUnclear(t *testing.T) {
	t.Parallel()
	direction, reason := testRules().NormalizeVerdict(NicheVerdict{Direction: "casino", OffProfileReason: "gambling"})
	if direction != NicheUnclear || reason != "" {
		t.Fatalf("NormalizeVerdict(casino, gambling) = (%q, %q), want (%q, \"\")", direction, reason, NicheUnclear)
	}
}

func TestNicheKnown_BoundToCurrentService(t *testing.T) {
	t.Parallel()
	rules := testRules()
	facts := Merge(Facts{}, FactDelta{Service: ptr("сеть магазинов")})
	if NicheKnown(facts) {
		t.Fatal("NicheKnown before verdict = true, want false")
	}
	rules.ApplyVerdict(facts, NicheVerdict{Direction: "retail"})
	if !NicheKnown(facts) {
		t.Fatal("NicheKnown after verdict = false, want true")
	}
	corrected := Merge(facts, FactDelta{Service: ptr("онлайн-казино")})
	if NicheKnown(corrected) {
		t.Fatal("NicheKnown after service correction = true, want false")
	}
}

func TestDiffer_IgnoresNicheVerdict(t *testing.T) {
	t.Parallel()
	rules := testRules()
	base := Merge(Facts{}, FactDelta{Service: ptr("сеть магазинов")})
	withVerdict := Merge(base, FactDelta{})
	rules.ApplyVerdict(withVerdict, NicheVerdict{Direction: "retail"})
	if Differ(base, withVerdict) {
		t.Error("Differ after verdict only = true, want false")
	}
	if !Differ(base, Merge(base, FactDelta{City: ptr("Омск")})) {
		t.Error("Differ after new city = false, want true")
	}
}

func TestMerge_CorrectionWinsAndNilKeeps(t *testing.T) {
	t.Parallel()
	base := Merge(Facts{}, FactDelta{Service: ptr("сеть магазинов"), City: ptr("Томск")})
	merged := Merge(base, FactDelta{City: ptr("Омск")})
	if got := merged.Value(FactCity); got != "Омск" {
		t.Errorf("city after correction = %q, want %q", got, "Омск")
	}
	if got := merged.Value(FactService); got != "сеть магазинов" {
		t.Errorf("service with nil delta = %q, want kept", got)
	}
	if got := base.Value(FactCity); got != "Томск" {
		t.Errorf("trusted snapshot changed: city = %q, want %q", got, "Томск")
	}
}

func TestOwnerRole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		base      FactDelta
		delta     FactDelta
		wantRole  string
		wantOwner bool // is_owner is known
	}{
		{"role owner does not set is_owner", FactDelta{}, FactDelta{Role: ptr(roleOwner)}, roleOwner, false},
		{"role director does not set is_owner", FactDelta{}, FactDelta{Role: ptr("директор")}, "директор", false},
		{"confirmed owner gets owner role", FactDelta{}, FactDelta{IsOwner: ptr(true)}, roleOwner, true},
		{"owner role does not overwrite real role", FactDelta{Role: ptr("директор"), IsOwner: ptr(true)},
			FactDelta{Role: ptr(roleOwner)}, "директор", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := Merge(Merge(Facts{}, tc.base), tc.delta)
			DeriveOwnerRole(facts)
			if got := facts.Value(FactRole); got != tc.wantRole {
				t.Errorf("role = %q, want %q", got, tc.wantRole)
			}
			if owner := !slices.Contains(facts.Missing(), FactIsOwner); owner != tc.wantOwner {
				t.Errorf("is_owner known = %v, want %v (facts %v)", owner, tc.wantOwner, facts)
			}
		})
	}
}

func TestCompleteIsIdempotent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status       Status
		factsChanged bool
		want         bool
	}{
		{StatusCollecting, false, false},
		{StatusReadyToDecide, false, true},
		{StatusReadyToDecide, true, false},
		{StatusMeetingScheduled, true, true},
	}
	for _, tc := range cases {
		if got := CompleteIsIdempotent(tc.status, tc.factsChanged); got != tc.want {
			t.Errorf("CompleteIsIdempotent(%s, factsChanged=%v) = %v, want %v", tc.status, tc.factsChanged, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		delta   FactDelta
		wantErr bool
	}{
		{"digits revenue", FactDelta{Revenue: ptr("3000000")}, false},
		{"range revenue", FactDelta{Revenue: ptr("800000-1200000")}, false},
		{"words revenue", FactDelta{Revenue: ptr("около трёх миллионов")}, true},
		{"overlong value", FactDelta{City: ptr(strings.Repeat("я", 1000))}, true},
	}
	for _, tc := range cases {
		err := Validate(Merge(Facts{}, tc.delta))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: Validate error = %v, want error %v", tc.name, err, tc.wantErr)
		}
	}
}
