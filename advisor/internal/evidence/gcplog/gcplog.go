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

// Package gcplog turns cluster-autoscaler visibility logs into evidence.
//
// Kubernetes events are ephemeral and carry no structured zone attribution, so
// they cannot say which zone refused a node. The visibility log can: its
// noScaleUp records carry rejectedMigs, whose names encode the machine type NAP
// tried to create and whose zone says where, plus napFailureReasons whose
// parameters name a zone but no shape. An observation needs both halves, so the
// two lists are read together.
//
// Not every refusal is a capacity refusal. The autoscaler reports "this pod
// does not fit this shape" in the same records as "this zone had nothing to
// give", and only the second belongs in a ledger keyed by (shape, zone). See
// aboutThePod.
package gcplog

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	logging "google.golang.org/api/logging/v2"

	"github.com/cwest/gke-spot-playbook/advisor/internal/evidence"
)

// logName is the GKE-managed stream that carries autoscaler decisions.
const logName = "container.googleapis.com%2Fcluster-autoscaler-visibility"

// scaleUpOutOfResources is the messageId for scale-up failures due to zone
// resource exhaustion.
const scaleUpOutOfResources = "scale.up.error.out.of.resources"

// maxEntries bounds one query. A tick every 10 minutes will never legitimately
// see more; the cap exists so a log storm cannot blow up memory.
const maxEntries = 500

// zoneRe matches a GCE zone and nothing else: a region ("us-central1",
// "europe-west12", "northamerica-northeast1") plus a single-letter suffix.
//
// It is anchored on purpose. Failure-reason parameters also carry node-pool
// names, and a loose match lets a pool called "pool-a100-x" pass for a zone and
// shadow the real one sitting in the next parameter.
var zoneRe = regexp.MustCompile(`^[a-z]+(?:-[a-z]+)*[0-9]{1,2}-[a-z]$`)

// migShapeRe pulls the machine type out of a NAP-created MIG name, which looks
// like "gke-<cluster>-nap-g2-standard-8-spot--<hash>-grp".
//
// The "nap-" segment is NOT at the start: GKE prefixes every MIG with
// "gke-<cluster>-". An "^nap-" anchor therefore matches nothing real, and
// because ParseEntry silently skips a MIG it cannot attribute, the failure
// mode is an evidence store that stays empty forever while looking exactly
// like a cluster that has had no preemptions.
//
// It is still anchored on a segment boundary rather than matching a bare
// "nap-" substring, so a pool called "snap-n2-standard-4" cannot be read as a
// NAP MIG and attributed to a shape NAP never tried to create.
//
// The grammar it accepts is family-series-size with a numeric size and nothing
// after it: "t2d-standard-8", "g2-standard-4", "n2-highmem-16". That covers
// every shape in advisor.yaml, and deliberately not the whole GCE namespace —
// "a2-ultragpu-1g" captures as "a2-ultragpu-1" and "e2-micro" does not match
// at all. Both failures are silent skips, the same class of bug the segment
// anchor above exists to fix, so widen this before putting either kind of
// shape on a rung.
var migShapeRe = regexp.MustCompile(`(?:^|-)nap-([a-z0-9]+-[a-z]+-[0-9]+)`)

// aboutThePod lists the noScaleUp reasons that describe the pending pod, or the
// cluster as a whole, rather than the (machine type, zone) pair the ledger is
// keyed by. They are not evidence about capacity and must never be stored.
//
// The distinction matters because an entry in the ledger drops that pair's
// weight to the floor — the same penalty as a hard stockout. "Your pod asked
// for 200 vCPU and this shape has 8" would then steer the ladder off a rung
// that is entirely healthy, and because the pod stays pending, every tick
// refreshes the observation and the decay never starts. The reconciler ends up
// permanently advising a region migration that cannot fix a pod-sizing problem.
// This is not hypothetical: it is what the Act 4 provocation actually did.
//
// Reasons are enumerated rather than allowlisted so that a capacity reason GKE
// adds later is read as evidence by default. A missed capacity signal leaves
// the prior in charge, which is merely uninformed; a fabricated one is wrong.
//
// Source: the noScaleUp reference in GKE's "cluster autoscaler visibility"
// documentation. Reasons and their meanings:
//   - mig.failing.predicate — an existing MIG failed a scheduling predicate
//     for the pod. Parameters carry the predicate name ("NodeResourcesFit").
//   - nap.pod.zonal.failing.predicates — the NAP-side twin: the pod would not
//     fit even a node pool NAP could create in that zone.
//   - mig.skipped — a MIG excluded during simulation, for a pod requirement or
//     a cluster ceiling ("max cluster cpu limit reached"). Neither is zonal.
//   - nap.pod.gpu.no.limit.defined — cluster-level GPU limits were never
//     configured. A cluster config fact, true in every zone.
//   - nap.disabled — NAP is off. Cluster-wide, and parameterless, so the zone
//     check below already drops it. Listed for completeness.
//   - in.backoff — the honest exception. This is per-node-group backoff after a
//     failed scale-up, and the failure behind it is often a real stockout, so
//     it is closer to capacity evidence than anything else on this list. It is
//     skipped anyway because backoff says a scale-up failed without saying why,
//     and the ledger's penalty is too blunt for a maybe. The genuine stockout
//     that caused it surfaces separately as scale.up.error.out.of.resources —
//     which this reader's filter does not currently collect (it selects only
//     noScaleUp), so that signal is a known gap, not something in.backoff is
//     standing in for.
//
// Filtering these is necessary but not sufficient, because one capacity reason
// is itself ambiguous — see doesNotFit.
var aboutThePod = map[string]bool{
	"no.scale.up.mig.failing.predicate":            true,
	"no.scale.up.nap.pod.zonal.failing.predicates": true,
	"no.scale.up.mig.skipped":                      true,
	"no.scale.up.nap.pod.gpu.no.limit.defined":     true,
	"no.scale.up.in.backoff":                       true,
	"no.scale.up.nap.disabled":                     true,
}

// logEntry represents a single entry from the Logging API before parse.
type logEntry struct {
	Payload []byte
	TS      time.Time
}

// decisionPayload mirrors the scale-up decision event structure.
type decisionPayload struct {
	Decision struct {
		EventID string `json:"eventId"`
		ScaleUp struct {
			IncreasedMigs []struct {
				Mig struct {
					Name string `json:"name"`
					Zone string `json:"zone"`
				} `json:"mig"`
			} `json:"increasedMigs"`
		} `json:"scaleUp"`
	} `json:"decision"`
}

// resultPayload mirrors the scale-up result event structure.
type resultPayload struct {
	ResultInfo struct {
		Results []struct {
			EventID  string `json:"eventId"`
			ErrorMsg struct {
				MessageID  string   `json:"messageId"`
				Parameters []string `json:"parameters"`
			} `json:"errorMsg"`
		} `json:"results"`
	} `json:"resultInfo"`
}

type Reader struct {
	svc                        *logging.Service
	project, cluster, location string
}

func New(ctx context.Context, project, cluster, location string) (*Reader, error) {
	svc, err := logging.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("logging client: %w", err)
	}
	return &Reader{svc: svc, project: project, cluster: cluster, location: location}, nil
}

// Filter builds the Logging query. Exported so it can be asserted on without
// a live API.
func Filter(project, cluster, location string, since time.Time) string {
	return strings.Join([]string{
		fmt.Sprintf(`logName="projects/%s/logs/%s"`, project, logName),
		fmt.Sprintf(`resource.labels.cluster_name=%q`, cluster),
		fmt.Sprintf(`resource.labels.location=%q`, location),
		`(jsonPayload.noDecisionStatus.noScaleUp:* OR jsonPayload.decision.scaleUp:* OR jsonPayload.resultInfo:*)`,
		fmt.Sprintf(`timestamp>=%q`, since.UTC().Format(time.RFC3339)),
	}, "\n")
}

// Refusals reads one page of visibility-log entries and returns every
// zone-attributed capacity refusal in it, drawn from both signals the filter
// selects: noScaleUp records (via ParseEntry, per entry) and out-of-resources
// scale-up failures (via scaleUpFailures, over the whole page, because a
// decision and its result are separate entries joined by eventId).
func (r *Reader) Refusals(ctx context.Context, since time.Time) ([]evidence.Observation, error) {
	req := &logging.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + r.project},
		Filter:        Filter(r.project, r.cluster, r.location, since),
		OrderBy:       "timestamp desc",
		PageSize:      int64(maxEntries),
	}
	resp, err := r.svc.Entries.List(req).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("list log entries: %w", err)
	}
	var entries []logEntry
	for _, e := range resp.Entries {
		if e.JsonPayload == nil {
			continue
		}
		b, err := e.JsonPayload.MarshalJSON()
		if err != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			ts = since
		}
		entries = append(entries, logEntry{Payload: b, TS: ts})
	}
	return merge(entries), nil
}

// merge turns one page of log entries into observations, drawing on both
// signals the filter selects: noScaleUp records (per entry, via ParseEntry) and
// out-of-resources scale-up failures (over the whole page at once, via
// scaleUpFailures, because a decision and its result are separate entries the
// join reunites by eventId). This is the reader's entire post-fetch behavior,
// factored out so the merge can be tested without a live Logging API.
func merge(entries []logEntry) []evidence.Observation {
	var out []evidence.Observation
	for _, e := range entries {
		out = append(out, ParseEntry(e.Payload, e.TS)...)
	}
	out = append(out, scaleUpFailures(entries)...)
	return out
}

// payload mirrors only the fields we read.
type payload struct {
	NoDecisionStatus struct {
		NoScaleUp struct {
			UnhandledPodGroups []struct {
				NapFailureReasons []struct {
					MessageID  string   `json:"messageId"`
					Parameters []string `json:"parameters"`
				} `json:"napFailureReasons"`
				RejectedMigs []struct {
					Mig struct {
						Name string `json:"name"`
						Zone string `json:"zone"`
					} `json:"mig"`
					// Every reason GKE documents at MIG level is a fact about
					// the pending pod, so none of them is evidence on its own.
					// The parameters are still worth decoding: they are what
					// distinguishes a pod that is too big for the shape from a
					// pod that was merely excluded by affinity or a taint. See
					// doesNotFit.
					Reason struct {
						MessageID  string   `json:"messageId"`
						Parameters []string `json:"parameters"`
					} `json:"reason"`
				} `json:"rejectedMigs"`
			} `json:"unhandledPodGroups"`
		} `json:"noScaleUp"`
	} `json:"noDecisionStatus"`
}

// ParseEntry extracts zone-attributed observations from one visibility-log
// payload. Anything it cannot attribute to both a shape and a zone is dropped:
// unattributed evidence would penalize the whole ladder indiscriminately.
//
// Every rejected MIG is paired with its own zone. A real noScaleUp record
// lists several MIGs across several zones, so keeping one MIG's shape while
// taking the zone from elsewhere would do double damage: the other MIGs'
// genuine failures are lost, and a pairing that never failed is written to the
// ledger. Evidence outranks the prior, so a fabricated observation steers the
// ladder off a rung that is actually healthy — the one thing this package must
// never do.
//
// A MIG supplies the shape and, when it has one, the authoritative zone; the
// pod-group reasons supply the reason and the fallback zone. One observation is
// emitted per usable reason per MIG. A record whose only reasons are pod facts
// yields nothing, which is the correct answer rather than a silent failure:
// nothing in it says anything about capacity.
func ParseEntry(b []byte, ts time.Time) []evidence.Observation {
	var p payload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil
	}
	var out []evidence.Observation
	for _, g := range p.NoDecisionStatus.NoScaleUp.UnhandledPodGroups {
		// Which shapes in this pod group the pod is simply too big for. Their
		// zonal exhaustion cannot be taken at face value; see doesNotFit.
		//
		// Suppression is per shape and total: a shape in here contributes no
		// observations from this pod group at all, not merely no zonal
		// exhaustion. That is broader than the enumerate-don't-allowlist bias
		// elsewhere in this file, and deliberately so — once the record has
		// said the pod does not fit this shape, no other reason in the same
		// group is a trustworthy statement about that shape's capacity.
		//
		// It is also narrow in one direction worth knowing about. A shape is
		// only marked when its OWN MIG carried the size rejection. If the pod
		// is too big for shape X but X's MIG happened to be rejected that tick
		// for some other reason, while a different shape's MIG carried the
		// NodeResourcesFit, X is not suppressed and can take a false
		// observation. Widening the trigger would suppress the case this
		// package most needs to see — a MIG rejected because the zone really
		// was empty — so the narrow rule stands and the decay curve absorbs
		// the occasional wrong guess.
		tooSmall := map[string]bool{}
		for _, m := range g.RejectedMigs {
			if s := migShapeRe.FindStringSubmatch(m.Mig.Name); len(s) == 2 &&
				doesNotFit(m.Reason.MessageID, m.Reason.Parameters) {
				tooSmall[s[1]] = true
			}
		}
		for _, m := range g.RejectedMigs {
			// The MIG name is the only place the machine type appears.
			s := migShapeRe.FindStringSubmatch(m.Mig.Name)
			if len(s) != 2 || tooSmall[s[1]] {
				continue
			}
			for _, reason := range g.NapFailureReasons {
				// A reason with no parameters makes no zonal claim at all
				// (no.scale.up.nap.disabled is a cluster-wide fact), so it is
				// not evidence against any one shape in any one zone.
				if len(reason.Parameters) == 0 {
					continue
				}
				// Nor is a reason that describes the pod rather than the pair.
				if aboutThePod[reason.MessageID] {
					continue
				}
				// A MIG is zonal, so its own zone is the authoritative half of
				// the pairing. The reason parameters are a fallback for the
				// MIGs that arrive without one.
				zone := m.Mig.Zone
				if zone == "" {
					zone = zoneParam(reason.Parameters)
				}
				if zone == "" {
					continue
				}
				out = append(out, evidence.Observation{
					MachineType: s[1],
					Zone:        zone,
					Reason:      reason.MessageID,
					At:          ts,
				})
			}
		}
	}
	return out
}

// doesNotFit reports whether a MIG's rejection says the pending pod is too big
// for that shape, as opposed to being excluded from it for some other reason.
//
// This is the discriminator for the one ambiguous capacity reason.
// no.scale.up.nap.pod.zonal.resources.exceeded folds three causes into a single
// message — the zone was short on resources, a cluster-wide maximum was hit, or
// no machine type could fit the request — and its only parameter is the zone.
// Read naively, the third cause makes an oversized pod look like a region-wide
// stockout: NAP reports every zone exhausted, because the pod fits nowhere.
//
// The record disambiguates itself. A pod that no shape can hold also fails
// NodeResourcesFit against the MIGs of that shape that already exist, and a
// zone that genuinely had nothing to give would not simultaneously be reporting
// that the pod is too large. So zonal exhaustion is only believed for a shape
// whose own MIGs did not reject the pod on size.
//
// The predicate name matters. NodeAffinity or PodToleratesNodeTaints means the
// pod was steered away from this shape, which says nothing about the zone's
// capacity — suppressing on those would discard real evidence.
//
// This is not a hypothetical: it is what the Act 4 provocation does. A 200-vCPU
// pod against an 8-vCPU ladder yields zonal.resources.exceeded for all four
// zones plus NodeResourcesFit on both live MIGs, and before this check the
// ledger condemned two healthy zones and held us-central1 at 0.047.
func doesNotFit(messageID string, params []string) bool {
	if messageID != "no.scale.up.mig.failing.predicate" &&
		messageID != "no.scale.up.nap.pod.zonal.failing.predicates" {
		return false
	}
	for _, p := range params {
		if p == "NodeResourcesFit" {
			return true
		}
	}
	return false
}

// migName reduces a MIG ID (bare name or full resource URL) to its trailing
// name segment, so a decision's increasedMigs name and a result's failing-MIG
// parameter compare regardless of which form the API used. Task 1 confirms the
// form; suffix reduction is correct for both.
func migName(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// scaleUpFailures joins scale-up decisions to their eventResults across a page
// of log entries and emits one (shape, zone) observation per failing MIG whose
// scale-up was refused for out-of-resources. Anything it cannot attribute to
// both a shape and a zone is dropped.
func scaleUpFailures(entries []logEntry) []evidence.Observation {
	// eventId -> failing-MIG-name -> zone
	zoneByEventMig := map[string]map[string]string{}
	for _, e := range entries {
		var d decisionPayload
		if json.Unmarshal(e.Payload, &d) != nil || d.Decision.EventID == "" {
			continue
		}
		m := zoneByEventMig[d.Decision.EventID]
		if m == nil {
			m = map[string]string{}
			zoneByEventMig[d.Decision.EventID] = m
		}
		for _, im := range d.Decision.ScaleUp.IncreasedMigs {
			m[migName(im.Mig.Name)] = im.Mig.Zone
		}
	}
	var out []evidence.Observation
	for _, e := range entries {
		var rp resultPayload
		if json.Unmarshal(e.Payload, &rp) != nil {
			continue
		}
		for _, res := range rp.ResultInfo.Results {
			if res.ErrorMsg.MessageID != scaleUpOutOfResources {
				continue
			}
			migs := zoneByEventMig[res.EventID] // nil if orphan → no zones
			for _, param := range res.ErrorMsg.Parameters {
				name := migName(param)
				zone, ok := migs[name]
				if !ok || zone == "" {
					continue
				}
				s := migShapeRe.FindStringSubmatch(name)
				if len(s) != 2 {
					continue
				}
				out = append(out, evidence.Observation{
					MachineType: s[1], Zone: zone,
					Reason: scaleUpOutOfResources, At: e.TS,
				})
			}
		}
	}
	return out
}

// zoneParam returns the first parameter that is exactly a GCE zone. It keeps
// scanning past a non-zone parameter rather than stopping at the first
// zone-shaped token, because reasons routinely lead with a node-pool name.
func zoneParam(params []string) string {
	for _, param := range params {
		if zoneRe.MatchString(param) {
			return param
		}
	}
	return ""
}
