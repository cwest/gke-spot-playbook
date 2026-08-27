// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package reconcile is one pass of the control loop: read state, ingest
// evidence if anything is waiting, re-score every profile, and apply the ladder
// only when the change clears hysteresis.
//
// The governing rule is that a failed tick must leave the previously applied
// ladder exactly as it was. Every failable step is caught and reported rather
// than returned, because a partial ladder is worse than a stale one.
package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/cwest/gke-spot-playbook/advisor/internal/advice"
	"github.com/cwest/gke-spot-playbook/advisor/internal/analyze"
	"github.com/cwest/gke-spot-playbook/advisor/internal/config"
	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence"
	"github.com/cwest/gke-spot-playbook/advisor/internal/kube"
	"github.com/cwest/gke-spot-playbook/advisor/internal/probe"
	"github.com/cwest/gke-spot-playbook/advisor/internal/render"
)

// LogSource yields observed provisioning failures newer than `since`.
type LogSource interface {
	Refusals(ctx context.Context, since time.Time) ([]evidence.Observation, error)
}

// eventNamespace is where every event this reconciler emits must live.
//
// All of them are about a ComputeClass, which is cluster-scoped, so their
// involvedObject.namespace is empty — and the API server accepts an Event with
// an empty involvedObject.namespace only in "default". Anywhere else the create
// is rejected with:
//
//	involvedObject.namespace: Invalid value: "": does not match event.namespace
//
// This is deliberately NOT Deps.Namespace. That one is the pod's own namespace,
// which holds the state ConfigMap; using it here silently loses every event,
// because an emit failure only appends a warning and the tick still reports
// success. Setting involvedObject.namespace to the pod namespace would satisfy
// the validator but lie about where the ComputeClass lives, and would break
// `kubectl describe computeclass`.
const eventNamespace = "default"

type Deps struct {
	API           advice.API
	Kube          kube.Client
	Log           LogSource // nil disables evidence ingestion
	Cfg           *config.Config
	Namespace     string
	StateName     string
	ClusterRegion string // where the cluster actually runs; drives the advisory
	Now           time.Time
	DryRun        bool
	Probe         probe.Compute // nil disables all probe automation
	ProbeCfg      config.Probe
}

type Result struct {
	Applied      []string
	NoOp         []string
	Waiting      []string
	Would        []string // dry-run: what would have been applied
	Warnings     []string
	Advisory     string
	Observations int // count of distinct shape/zone pairs learned about this tick
}

// Applied records what is currently live for a class.
type Applied struct {
	Fingerprint string    `json:"fingerprint"`
	Score       float64   `json:"score"`
	Region      string    `json:"region"`
	At          time.Time `json:"at"`
	Evidence    bool      `json:"evidence"` // true if this ladder went live via the fast path
}

// PendingChange is a candidate ladder that has not yet cleared hysteresis.
type PendingChange struct {
	Fingerprint string  `json:"fingerprint"`
	Ticks       int     `json:"ticks"`
	Evidence    bool    `json:"evidence"`
	Score       float64 `json:"score"`
	Region      string  `json:"region"`
}

type State struct {
	Ledger             *evidence.Ledger             `json:"ledger"`
	Applied            map[string]Applied           `json:"applied"`
	Pending            map[string]PendingChange     `json:"pending"`
	LastAdvisory       string                       `json:"lastAdvisory,omitempty"`
	LastLogQuery       time.Time                    `json:"lastLogQuery"`
	ProbeConfirmations map[string]ProbeConfirmation `json:"probeConfirmations,omitempty"`
	PendingProbes      []PendingProbe               `json:"pendingProbes,omitempty"`
	ProbeBudget        ProbeBudget                  `json:"probeBudget"`
}

// probeAutomated reports whether the reconciler's probe automation is wired and
// enabled. Every probe side effect (reap, gate, launch) is guarded by it so a
// deployment without a probe client or with automation off behaves exactly as
// it did before this path existed.
func probeAutomated(d Deps) bool {
	return d.Probe != nil && d.ProbeCfg.Automated
}

func newState() *State {
	return &State{
		Ledger:             evidence.NewLedger(),
		Applied:            map[string]Applied{},
		Pending:            map[string]PendingChange{},
		ProbeConfirmations: map[string]ProbeConfirmation{},
	}
}

func decodeState(b []byte) *State {
	if len(b) == 0 {
		return newState()
	}
	s := &State{}
	if err := json.Unmarshal(b, s); err != nil {
		// A corrupt state document is recoverable: the worst case is one
		// unnecessary re-apply of a ladder we would have applied anyway.
		return newState()
	}
	if s.Ledger == nil || s.Ledger.Latest == nil {
		s.Ledger = evidence.NewLedger()
	}
	if s.Applied == nil {
		s.Applied = map[string]Applied{}
	}
	if s.Pending == nil {
		s.Pending = map[string]PendingChange{}
	}
	if s.ProbeConfirmations == nil {
		s.ProbeConfirmations = map[string]ProbeConfirmation{}
	}
	// Applied.Evidence defaults to false for state documents created before
	// this field was added. On first widen after an old document, the fast
	// path does not fire and recovery takes one extra tick; then the field
	// is set correctly on the next apply and self-corrects.
	return s
}

// inRegion narrows an analysis to the cluster's own region. The ladder is
// applied to ONE regional cluster, so a rung in another region renders
// location.zones that GKE Warden rejects outright; the cross-region comparison
// is adviseRegion's job, not the ladder's. A blank region means "no cluster
// context" (the one-shot paths) and narrows nothing.
func inRegion(a *analyze.Analysis, region string) *analyze.Analysis {
	if region == "" {
		return a
	}
	out := *a
	out.Candidates = nil
	for _, c := range a.Candidates {
		if c.Region == region {
			out.Candidates = append(out.Candidates, c)
		}
	}
	return &out
}

// Fingerprint hashes the semantic content of a manifest. Comment lines are
// excluded because the renderer stamps GeneratedAt into the header: hashing
// them would make every tick look like a change.
func Fingerprint(manifest []byte) string {
	h := sha256.New()
	for _, line := range strings.Split(string(manifest), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		h.Write([]byte(line))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func Tick(ctx context.Context, d Deps, profiles []string) (*Result, error) {
	if d.Cfg == nil || d.Kube == nil || d.API == nil {
		return nil, fmt.Errorf("reconcile: Cfg, Kube and API are required")
	}
	raw, err := d.Kube.GetState(ctx, d.Namespace, d.StateName)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	st := decodeState(raw)
	res := &Result{}

	// Reap any probes launched on an earlier tick before scoring: a probe that
	// has just reached RUNNING writes a confirmation the widen/promote gate below
	// consults this same tick.
	if probeAutomated(d) {
		reapProbes(ctx, d, st)
	}

	params := d.Cfg.Evidence.Params()

	type target struct{ profile, class string }
	targets := make([]target, 0, len(profiles))
	classes := make([]string, 0, len(profiles))
	for _, name := range profiles {
		p, ok := d.Cfg.Profiles[name]
		if !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("unknown profile %q: skipped", name))
			continue
		}
		targets = append(targets, target{profile: name, class: "batch-" + p.Kind})
		classes = append(classes, "batch-"+p.Kind)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no requested profile exists in the config: %v", profiles)
	}

	ingest(ctx, d, st, res, classes, params)
	st.Ledger.Prune(d.Now, params.MaxAge)
	src := analyze.LedgerSource(st.Ledger, d.Now, params)

	var advisories []string
	for _, t := range targets {
		a, err := analyze.Run(ctx, d.API, d.Cfg, t.profile, d.Now, src)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: analyze: %v", t.profile, err))
			continue // leave the live ladder alone
		}
		if adv := adviseRegion(a.Candidates, d.ClusterRegion, d.Cfg.Hysteresis.MinScoreDelta); adv != "" {
			advisories = append(advisories, adv)
		}
		// adviseRegion above compares every region; everything below is about
		// this cluster's own ladder and must not leave its region.
		local := inRegion(a, d.ClusterRegion)
		// Gate both widening seams on live probe confirmations: a dry *sharded*
		// zone rejoins its rung only once a probe confirms it (gateWidenPromote),
		// and an all-dry rung widens onto a never-sampled zone only once a probe
		// confirms it (gateWiden + ComputeClassGated). Either way an unconfirmed
		// zone earns a probe rather than being widened onto blind.
		// With automation off there are no probe confirmations, so the gate must
		// be always-true to reproduce render.ComputeClass's widening exactly;
		// gating on confirmed() there would suppress every never-sharded widen and
		// silently collapse an all-dry rung to its pinned (known-dry) zones.
		gate := func(mt, z string) bool { return true }
		if probeAutomated(d) {
			gateWidenPromote(ctx, d, st, local)
			gateWiden(ctx, d, st, local, d.ClusterRegion)
			gate = func(mt, z string) bool { return confirmed(st, mt, z, d.Now) }
		}
		y, err := render.ComputeClassGated(local, t.class, d.Cfg.Caps.MaxSpotRungs, gate)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: render: %v", t.profile, err))
			continue
		}
		reconcileClass(ctx, d, st, res, t.class, local, y)
	}

	res.Advisory = strings.Join(advisories, "; ")
	publishAdvisory(ctx, d, st, res)

	if !d.DryRun {
		doc, err := json.Marshal(st)
		if err != nil {
			return res, fmt.Errorf("encode state: %w", err)
		}
		if err := d.Kube.PutState(ctx, d.Namespace, d.StateName, doc); err != nil {
			return res, fmt.Errorf("write state: %w", err)
		}
	}
	return res, nil
}

// ingest runs the two-step evidence gate. Step one is a cheap in-cluster pod
// count; only if something is actually waiting for capacity do we pay for the
// Cloud Logging query in step two.
func ingest(ctx context.Context, d Deps, st *State, res *Result, classes []string, p evidence.Params) {
	if d.Log == nil {
		return
	}
	pending, err := d.Kube.PendingClassPods(ctx, classes)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("pending pods: %v", err))
		return
	}
	if pending == 0 {
		return
	}
	since := d.Now.Add(-p.MaxAge)
	if st.LastLogQuery.After(since) {
		since = st.LastLogQuery
	}
	obs, err := d.Log.Refusals(ctx, since)
	if err != nil {
		// Stale evidence beats no ladder update at all.
		res.Warnings = append(res.Warnings, fmt.Sprintf("log query: %v", err))
		return
	}
	seen := make(map[evidence.Key]struct{}, len(obs))
	for _, o := range obs {
		st.Ledger.Add(o)
		seen[evidence.Key{MachineType: o.MachineType, Zone: o.Zone}] = struct{}{}
	}
	st.LastLogQuery = d.Now
	res.Observations = len(seen)
}

// reconcileClass decides whether one class's newly rendered ladder is allowed
// to go live.
func reconcileClass(ctx context.Context, d Deps, st *State, res *Result,
	class string, a *analyze.Analysis, y []byte) {

	fp := Fingerprint(y)
	live := st.Applied[class]
	if fp == live.Fingerprint {
		delete(st.Pending, class)
		res.NoOp = append(res.NoOp, class)
		return
	}

	byEvidence := hasEvidence(a) || live.Evidence
	top, region, ok := topOf(a)
	if !ok {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: no viable candidates", class))
		return
	}

	pc := st.Pending[class]
	if pc.Fingerprint != fp {
		pc = PendingChange{Fingerprint: fp}
	}
	pc.Ticks++
	pc.Evidence = byEvidence
	pc.Score = top
	pc.Region = region

	required := d.Cfg.Hysteresis.ConsecutiveTicks
	if byEvidence {
		// A zone that just refused a node should not linger on the ladder for
		// three more ticks.
		required = d.Cfg.Evidence.FastPathTicks
	}
	if live.Fingerprint == "" {
		// Cold start: a cluster with no ladder gets one now, not in three ticks.
		// There is nothing working to protect, so hysteresis has nothing to buy.
		required = 1
	}
	if pc.Ticks < required {
		st.Pending[class] = pc
		res.Waiting = append(res.Waiting, class)
		return
	}
	// Score-driven churn additionally has to be worth it. Evidence skips this:
	// the delta bar measures shifted estimates, and an observed hard failure is
	// not an estimate.
	if !byEvidence && !worthIt(live.Score, top, d.Cfg.Hysteresis.MinScoreDelta) {
		st.Pending[class] = pc
		res.Waiting = append(res.Waiting, class)
		return
	}
	if d.DryRun {
		res.Would = append(res.Would, class)
		return
	}
	if err := d.Kube.ApplyComputeClass(ctx, y); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: apply: %v", class, err))
		return // the live ladder is untouched
	}
	st.Applied[class] = Applied{Fingerprint: fp, Score: top, Region: region, At: d.Now, Evidence: hasEvidence(a)}
	delete(st.Pending, class)
	res.Applied = append(res.Applied, class)

	reason, msg := "LadderRescored", fmt.Sprintf("re-ranked %s (top score %.3f in %s)", class, top, region)
	switch {
	case hasEvidence(a):
		reason = "LadderEvidenceUpdate"
		msg = fmt.Sprintf("observed provisioning failures re-ranked %s (top score %.3f in %s)", class, top, region)
	case live.Evidence:
		reason = "LadderEvidenceRecovered"
		msg = fmt.Sprintf("previously failing capacity recovered; widened %s (top score %.3f in %s)", class, top, region)
	}
	if err := d.Kube.EmitEvent(ctx, eventNamespace, kube.Event{
		Reason: reason, Message: msg, Type: "Normal", InvolvedName: class,
	}); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: event: %v", class, err))
	}
}

func hasEvidence(a *analyze.Analysis) bool {
	for _, c := range a.Candidates {
		if c.EvidenceDry {
			return true
		}
	}
	return false
}

// topOf returns the representative score and region for a ladder. It prefers
// kept candidates: a dropped candidate carries Composite 0, and persisting that
// as the live score would make worthIt's live <= 0 branch disable the delta bar
// permanently. ok is false when nothing survived scoring.
func topOf(a *analyze.Analysis) (score float64, region string, ok bool) {
	for _, c := range a.Candidates {
		if !c.Dropped {
			return c.Composite, c.Region, true
		}
	}
	return 0, "", false
}

// worthIt reports whether the new top score differs from the live one by enough to
// justify churning node pools. The delta is symmetric: a materially worse top
// score should trigger a re-rank because the ladder below it has moved. A cold
// start (live == 0) always qualifies.
func worthIt(live, next, minDelta float64) bool {
	if live <= 0 {
		return true
	}
	return math.Abs(next-live)/live >= minDelta
}

// adviseRegion suggests a move when another region clearly beats the one the
// cluster runs in. The reconciler never moves the cluster itself: a region move
// destroys and rebuilds it, which is a human's call.
//
// Among the regions that clear the bar, the cheapest wins rather than the
// highest-scoring. Every candidate past the bar is good enough to run the
// workload, so the tie-break that matters is the bill.
func adviseRegion(cands []analyze.Candidate, current string, minDelta float64) string {
	type summary struct {
		region  string
		best    float64
		perUnit float64
	}
	byRegion := map[string]*summary{}
	var order []string
	for _, c := range cands {
		if c.Dropped {
			continue
		}
		s, ok := byRegion[c.Region]
		if !ok {
			s = &summary{region: c.Region}
			byRegion[c.Region] = s
			order = append(order, c.Region)
		}
		if c.Composite > s.best {
			s.best = c.Composite
			if c.Units > 0 {
				s.perUnit = c.SpotHourlyUSD / c.Units
			}
		}
	}
	here, ok := byRegion[current]
	if !ok || here.best <= 0 {
		return "" // nothing to compare against
	}
	var better []*summary
	for _, r := range order {
		s := byRegion[r]
		if r == current {
			continue
		}
		if (s.best-here.best)/here.best >= minDelta {
			better = append(better, s)
		}
	}
	if len(better) == 0 {
		return ""
	}
	sort.Slice(better, func(i, j int) bool {
		if better[i].perUnit != better[j].perUnit {
			return better[i].perUnit < better[j].perUnit
		}
		return better[i].region < better[j].region
	})
	w := better[0]
	return fmt.Sprintf(
		"%s scores %.3f vs %.3f here at $%.4f/unit-hr — run: infra/migrate-region.sh %s",
		w.region, w.best, here.best, w.perUnit, w.region)
}

// publishAdvisory emits the advisory as a Warning event, but only when it
// changes. A CronJob every 10 minutes would otherwise bury the event log.
func publishAdvisory(ctx context.Context, d Deps, st *State, res *Result) {
	if res.Advisory == st.LastAdvisory {
		return
	}
	if res.Advisory == "" || d.DryRun {
		st.LastAdvisory = res.Advisory
		return
	}
	if err := d.Kube.EmitEvent(ctx, eventNamespace, kube.Event{
		Reason: "RegionAdvisory", Message: res.Advisory, Type: "Warning", InvolvedName: "batch-cpu",
	}); err != nil {
		// Leave LastAdvisory alone so the next tick retries; the advisory string
		// does not change while the same region leads, so recording it here
		// would silence it for good.
		res.Warnings = append(res.Warnings, fmt.Sprintf("advisory event: %v", err))
		return
	}
	st.LastAdvisory = res.Advisory
}
