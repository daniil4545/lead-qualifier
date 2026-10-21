package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/daniil4545/lead-qualifier/internal/config"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

type failingClassifier struct{ failure *ExternalFailure }

func (c failingClassifier) ClassifyNiche(context.Context, NicheInput) (qualify.NicheVerdict, error) {
	return qualify.NicheVerdict{}, c.failure
}

// A refusal because of a classifier failure is forbidden: a permanent failure gives an unconfirmed
// profile and an open gate, a repeatable one goes up and replays the turn.
func TestEnsureNiche_ClassifierFailure(t *testing.T) {
	t.Parallel()
	fx := loadFixture(t)

	t.Run("permanent failure opens gate", func(t *testing.T) {
		t.Parallel()
		facts := fx.fullFacts("3000000", "")
		err := EnsureNiche(context.Background(), failingClassifier{&ExternalFailure{Kind: "http", Permanent: true, Status: 401}},
			fx.rules, 1, facts, "У нас сеть магазинов.")
		if err != nil {
			t.Fatalf("EnsureNiche: %v, want nil", err)
		}
		if !qualify.NicheKnown(facts) || !fx.rules.Gate(facts).AllowsMeeting() {
			t.Fatalf("niche known = %v, gate = %s, want known and open", qualify.NicheKnown(facts), fx.rules.Gate(facts))
		}
	})
	t.Run("retryable failure returns error", func(t *testing.T) {
		t.Parallel()
		facts := fx.fullFacts("3000000", "")
		err := EnsureNiche(context.Background(), failingClassifier{&ExternalFailure{Kind: "http", Status: 503}},
			fx.rules, 1, facts, "У нас сеть магазинов.")
		if err == nil {
			t.Fatal("EnsureNiche error = nil, want retryable failure")
		}
	})
}

type nicheCase struct {
	service string
	want    qualify.Gate
	segment string // segment code for offer; empty for off_profile
}

// loadNicheCases reads niche_cases.tsv: description, gate outcome, catalog segment code
// or unclear (empty for off_profile), explanation.
func loadNicheCases(t *testing.T, catalog config.Catalog) []nicheCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(catalogDir, "niche_cases.tsv"))
	if err != nil {
		t.Fatalf("read niche cases: %v", err)
	}
	var cases []nicheCase
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 || strings.TrimSpace(fields[0]) == "" {
			t.Fatalf("line %d: want 4 tab-separated fields with a description, got %q", i+1, line)
		}
		gate := qualify.Gate(fields[1])
		if gate != qualify.GateOffer && gate != qualify.GateOffProfile {
			t.Fatalf("line %d: outcome %q, want %s or %s", i+1, fields[1], qualify.GateOffer, qualify.GateOffProfile)
		}
		if seen[fields[0]] {
			t.Fatalf("line %d: duplicate description %q", i+1, fields[0])
		}
		segment := fields[2]
		// unclear is the code of an unconfirmed profile: a meeting without a catalog segment.
		known := segment == qualify.NicheUnclear ||
			slices.ContainsFunc(catalog.Segments, func(s config.Segment) bool { return s.Code == segment })
		if (gate == qualify.GateOffer) != known || gate == qualify.GateOffProfile && segment != "" {
			t.Fatalf("line %d: outcome %s with segment %q, want a catalog segment or unclear for offer and none for off_profile",
				i+1, gate, segment)
		}
		seen[fields[0]] = true
		cases = append(cases, nicheCase{service: fields[0], want: gate, segment: segment})
	}
	return cases
}

// Offline check of the niche set: the format is valid and both gate outcomes are present.
func TestNicheCases_Format(t *testing.T) {
	t.Parallel()
	outcomes := map[qualify.Gate]int{}
	for _, c := range loadNicheCases(t, loadFixture(t).catalog) {
		outcomes[c.want]++
	}
	if outcomes[qualify.GateOffer] == 0 || outcomes[qualify.GateOffProfile] == 0 {
		t.Fatalf("outcomes %v, want both offer and off_profile cases", outcomes)
	}
}

// Live niche classifier test: each description with revenue above all
// thresholds yields a gate outcome from the set, and offer yields the expected segment.
// Needs LQ_LIVE_MODEL=1 and OPENROUTER_API_KEY.
func TestNicheClassifierLive(t *testing.T) {
	if testing.Short() || os.Getenv("LQ_LIVE_MODEL") != "1" || os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("live model test: set LQ_LIVE_MODEL=1 and OPENROUTER_API_KEY, run without -short")
	}
	fx := loadFixture(t)
	cfg, err := config.LoadOpenRouter(fx.catalog)
	if err != nil {
		t.Fatalf("LoadOpenRouter: %v", err)
	}
	client, err := FromCatalog(catalogDir, "", cfg, fx.catalog, nil)
	if err != nil {
		t.Fatalf("FromCatalog: %v", err)
	}
	var top int64
	for _, tariff := range fx.catalog.Tariffs {
		top = max(top, tariff.Threshold)
	}
	for _, segment := range fx.catalog.Segments {
		top = max(top, segment.Threshold)
	}
	for i, c := range loadNicheCases(t, fx.catalog) {
		facts := qualify.Merge(qualify.Facts{}, qualify.FactDelta{Service: strPtr(c.service),
			Revenue: strPtr(strconv.FormatInt(top, 10)), Role: strPtr("директор"), IsOwner: boolPtr(true), City: strPtr("Город")})
		if err := EnsureNiche(context.Background(), client, fx.rules, int64(i+1), facts, c.service); err != nil {
			t.Errorf("%q: EnsureNiche: %v", c.service, err)
			continue
		}
		gate := fx.rules.Gate(facts)
		if (c.want == qualify.GateOffProfile) != (gate == qualify.GateOffProfile) {
			t.Errorf("%q: gate %s, want %s", c.service, gate, c.want)
		}
		if got := facts.Value("niche"); c.want == qualify.GateOffer && got != c.segment {
			t.Errorf("%q: segment %q, want %q", c.service, got, c.segment)
		}
	}
}
