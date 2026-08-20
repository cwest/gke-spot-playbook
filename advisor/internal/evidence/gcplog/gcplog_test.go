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

package gcplog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/evidence"
)

// A trimmed but structurally faithful noScaleUp payload. The zone lives in
// parameters[0] of the NAP failure reason; the shape lives in the rejected
// migs/pod group. GKE emits one entry per unhandled pod group.
const samplePayload = `{
  "noDecisionStatus": {
    "measureTime": "1780000000",
    "noScaleUp": {
      "unhandledPodGroups": [
        {
          "napFailureReasons": [
            {
              "messageId": "no.scale.up.nap.pod.zonal.resources.exceeded",
              "parameters": ["us-central1-a"]
            }
          ],
          "podGroup": {
            "samplePod": {"name": "tune-worker-abc", "namespace": "default"}
          },
          "rejectedMigs": [
            {"mig": {"name": "nap-g2-standard-4-xyz", "zone": "us-central1-a"}}
          ]
        }
      ]
    }
  }
}`

func TestParseEntryExtractsZoneAndShape(t *testing.T) {
	ts := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	got := ParseEntry([]byte(samplePayload), ts)
	if len(got) != 1 {
		t.Fatalf("got %d observations, want 1: %+v", len(got), got)
	}
	o := got[0]
	if o.Zone != "us-central1-a" {
		t.Errorf("Zone = %q, want us-central1-a", o.Zone)
	}
	if o.MachineType != "g2-standard-4" {
		t.Errorf("MachineType = %q, want g2-standard-4", o.MachineType)
	}
	if !strings.Contains(o.Reason, "zonal.resources.exceeded") {
		t.Errorf("Reason = %q, want it to mention zonal.resources.exceeded", o.Reason)
	}
	if !o.At.Equal(ts) {
		t.Errorf("At = %v, want %v", o.At, ts)
	}
}

func TestParseEntryIgnoresNonNoScaleUpPayloads(t *testing.T) {
	// Structurally complete: the same pod group, reasons and migs as the
	// noScaleUp sample, but hung under decision.scaleUp. Only the
	// noDecisionStatus.noScaleUp discriminator keeps this at zero, so an
	// implementation that walked any unhandledPodGroups it could find would
	// record a *successful* scale-up as a failure.
	payload := `{"decision":{"scaleUp":{"unhandledPodGroups":[{
	  "napFailureReasons":[{"messageId":"no.scale.up.nap.pod.zonal.resources.exceeded",
	    "parameters":["us-central1-a"]}],
	  "rejectedMigs":[{"mig":{"name":"nap-g2-standard-4-xyz","zone":"us-central1-a"}}]}]}}}`
	if got := ParseEntry([]byte(payload), time.Now()); len(got) != 0 {
		t.Fatalf("got %d observations from a scaleUp payload, want 0: %+v", len(got), got)
	}
}

func TestParseEntryDropsUndecodableJSON(t *testing.T) {
	tests := map[string]string{
		// Nothing decodes at all.
		"malformed": `{not json`,
		// Everything decodes except messageId, which is a number. encoding/json
		// records the type error and keeps populating the siblings, so without
		// the error guard this yields a fully-formed observation with an empty
		// Reason: evidence invented out of a payload we did not understand.
		"type mismatch": `{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
		  "napFailureReasons":[{"messageId":42,"parameters":["us-central1-a"]}],
		  "rejectedMigs":[{"mig":{"name":"nap-g2-standard-4-xyz","zone":"us-central1-a"}}]}]}}}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if got := ParseEntry([]byte(payload), time.Now()); len(got) != 0 {
				t.Fatalf("got %d observations from undecodable JSON, want 0: %+v", len(got), got)
			}
		})
	}
}

func TestParseEntrySkipsReasonsWithoutAZoneParameter(t *testing.T) {
	payload := `{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
	  "napFailureReasons":[{"messageId":"no.scale.up.nap.disabled","parameters":[]}],
	  "rejectedMigs":[{"mig":{"name":"nap-g2-standard-4-xyz","zone":"us-central1-a"}}]}]}}}`
	if got := ParseEntry([]byte(payload), time.Now()); len(got) != 0 {
		t.Fatalf("got %d observations without a zone parameter, want 0: %+v", len(got), got)
	}
}

func TestParseEntryPrefersTheMigZoneOverANonZoneParameter(t *testing.T) {
	// Some reasons carry a node-pool name where the zone would be. The MIG is
	// zonal, so its own zone settles the pairing.
	payload := `{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
	  "napFailureReasons":[{"messageId":"no.scale.up.nap.pod.zonal.gpu.type.not.supported",
	    "parameters":["nap-g2-standard-8-pool"]}],
	  "rejectedMigs":[{"mig":{"name":"nap-g2-standard-8-pool","zone":"us-central1-c"}}]}]}}}`
	got := ParseEntry([]byte(payload), time.Now())
	if len(got) != 1 {
		t.Fatalf("got %d observations, want 1: %+v", len(got), got)
	}
	if got[0].Zone != "us-central1-c" {
		t.Errorf("Zone = %q, want us-central1-c from the MIG", got[0].Zone)
	}
	if got[0].MachineType != "g2-standard-8" {
		t.Errorf("MachineType = %q, want g2-standard-8", got[0].MachineType)
	}
	// Reason must be carried through from the payload, not assumed.
	if want := "no.scale.up.nap.pod.zonal.gpu.type.not.supported"; got[0].Reason != want {
		t.Errorf("Reason = %q, want %q", got[0].Reason, want)
	}
}

func TestParseEntryPairsEachMigWithItsOwnZone(t *testing.T) {
	// A real noScaleUp record lists every MIG NAP considered, across zones and
	// shapes. Keeping only the first MIG's shape while taking the zone from the
	// failure reason both loses a2-highgpu-1@us-central1-b and fabricates
	// g2-standard-4@us-central1-b, which never failed.
	payload := `{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
	  "napFailureReasons":[{"messageId":"no.scale.up.nap.pod.zonal.resources.exceeded",
	    "parameters":["us-central1-b"]}],
	  "rejectedMigs":[
	    {"mig":{"name":"nap-g2-standard-4-xyz","zone":"us-central1-a"}},
	    {"mig":{"name":"nap-a2-highgpu-1-abc","zone":"us-central1-b"}}]}]}}}`
	got := ParseEntry([]byte(payload), time.Now())
	if len(got) != 2 {
		t.Fatalf("got %d observations, want 2: %+v", len(got), got)
	}
	want := map[string]bool{
		"g2-standard-4@us-central1-a": true,
		"a2-highgpu-1@us-central1-b":  true,
	}
	for _, o := range got {
		pair := o.MachineType + "@" + o.Zone
		if !want[pair] {
			t.Errorf("unexpected observation %s", pair)
		}
		delete(want, pair)
	}
	for pair := range want {
		t.Errorf("missing observation %s", pair)
	}
}

func TestParseEntryIgnoresAZoneShapedNodePoolName(t *testing.T) {
	// The MIG carries no zone, so the parameters are the only source. The first
	// parameter is a node-pool name that is shaped like a zone; the real zone is
	// behind it. Stopping at the first zone-shaped token loses the real one.
	payload := `{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
	  "napFailureReasons":[{"messageId":"no.scale.up.nap.pod.zonal.resources.exceeded",
	    "parameters":["pool-a100-x","us-central1-a"]}],
	  "rejectedMigs":[{"mig":{"name":"nap-g2-standard-4-xyz"}}]}]}}}`
	got := ParseEntry([]byte(payload), time.Now())
	if len(got) != 1 {
		t.Fatalf("got %d observations, want 1: %+v", len(got), got)
	}
	if got[0].Zone != "us-central1-a" {
		t.Errorf("Zone = %q, want us-central1-a (not the node-pool name)", got[0].Zone)
	}
}

func TestParseEntryReadsRealNapMigNames(t *testing.T) {
	// Every other fixture in this file uses "nap-<shape>-<hash>", a format GKE
	// does not actually emit. A real NAP MIG name, copied verbatim from this
	// project's cluster-autoscaler-visibility logs, is
	// "gke-<cluster>-nap-<shape>-<suffix>--<hash>-grp". An "^nap-" anchor never
	// matches it, so ParseEntry drops every observation and the evidence store
	// stays empty forever — indistinguishable from "no preemptions yet".
	payload := `{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
	  "napFailureReasons":[{"messageId":"no.scale.up.nap.pod.zonal.resources.exceeded",
	    "parameters":["us-central1-c"]}],
	  "rejectedMigs":[{"mig":{"name":"gke-spot-demo-nap-g2-standard-8-spot--a1453634-grp",
	    "zone":"us-central1-c"}}]}]}}}`
	got := ParseEntry([]byte(payload), time.Now())
	if len(got) != 1 {
		t.Fatalf("got %d observations, want 1: %+v", len(got), got)
	}
	if got[0].MachineType != "g2-standard-8" {
		t.Errorf("MachineType = %q, want g2-standard-8", got[0].MachineType)
	}
	if got[0].Zone != "us-central1-c" {
		t.Errorf("Zone = %q, want us-central1-c", got[0].Zone)
	}
}

func TestParseEntryIgnoresAPredicateRejectionOnTheMig(t *testing.T) {
	// Copied verbatim from this cluster's visibility log on 2026-08-02, minus
	// the second MIG. It is what GKE emits when a MIG is considered and
	// rejected: the reason hangs off the *MIG*, and there is no pod-group-level
	// napFailureReasons list at all.
	//
	// It is tempting to read, because the shape and the zone are both right
	// there on the MIG — better attributed than any pod-group reason. But
	// "no.scale.up.mig.failing.predicate" with a NodeResourcesFit parameter is
	// a fact about the pending *pod*: it asked for 200 vCPU and a
	// t2d-standard-8 has 8. It says nothing about whether t2d-standard-8 spot
	// capacity is obtainable in us-central1-b.
	//
	// The ledger is keyed by (shape, zone), and an entry in it drops that
	// pair's weight to the floor. Writing a pod-fit failure there steers the
	// ladder off a rung that is perfectly healthy, which is worse than
	// recording nothing: the pod stays pending, so every tick refreshes the
	// observation, the decay never starts, and the reconciler advises a region
	// migration that cannot possibly help.
	payload := `{"noDecisionStatus":{"measureTime":"1785695437","noScaleUp":{
	  "unhandledPodGroups":[{
	    "podGroup":{"samplePod":{"name":"provoke-noscaleup","namespace":"spot-demo"},
	      "totalPodCount":1},
	    "rejectedMigs":[{
	      "mig":{"name":"gke-spot-demo-nap-t2d-standard-8-spot-c553209c-grp",
	        "nodepool":"nap-t2d-standard-8-spot-56pkis16","zone":"us-central1-b"},
	      "reason":{"messageId":"no.scale.up.mig.failing.predicate",
	        "parameters":["NodeResourcesFit","Insufficient cpu"]}}]}],
	  "unhandledPodGroupsTotalCount":1}}}`
	if got := ParseEntry([]byte(payload), time.Now()); len(got) != 0 {
		t.Fatalf("got %d observations from a predicate rejection, want 0: %+v", len(got), got)
	}
}

func TestParseEntryIgnoresPodFactReasonsFromThePodGroup(t *testing.T) {
	// The same distinction on the pod-group path, where the reason does carry a
	// zone and so would otherwise be fully attributable. Both of these name a
	// zone; neither is a fact about that zone's capacity.
	//
	// "no.scale.up.nap.pod.zonal.failing.predicates" is the NAP-side twin of
	// mig.failing.predicate: the pod would not fit even a node pool NAP could
	// create. It belongs in the pod-group list and this subtest is the
	// realistic one.
	//
	// "no.scale.up.mig.skipped" is a MIG-level reason and GKE is not expected
	// to emit it here; the subtest is defensive. aboutThePod is consulted for
	// pod-group reasons only, so a MIG-level id landing in that list — through
	// an API change, or a payload shape not yet observed — would otherwise be
	// unrecognised and, under the enumerate-don't-allowlist default, read as
	// capacity evidence. Skipping it excludes a MIG for a pod requirement or a
	// cluster-wide ceiling ("max cluster cpu limit reached"), neither of which
	// is zonal.
	for _, messageID := range []string{
		"no.scale.up.nap.pod.zonal.failing.predicates",
		"no.scale.up.mig.skipped",
	} {
		t.Run(messageID, func(t *testing.T) {
			payload := `{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
			  "napFailureReasons":[{"messageId":"` + messageID + `",
			    "parameters":["us-central1-b","NodeResourcesFit"]}],
			  "rejectedMigs":[{"mig":{"name":"gke-spot-demo-nap-t2d-standard-8-x-grp",
			    "zone":"us-central1-b"}}]}]}}}`
			if got := ParseEntry([]byte(payload), time.Now()); len(got) != 0 {
				t.Fatalf("got %d observations, want 0: %+v", len(got), got)
			}
		})
	}
}

func TestParseEntryIgnoresZonalExhaustionWhenThePodFitsNoShape(t *testing.T) {
	// Copied verbatim from this cluster's visibility log on 2026-08-02, with the
	// full four-zone reason list and both MIGs. This is the record the Act 4
	// provocation actually produces, and it is a trap.
	//
	// nap.pod.zonal.resources.exceeded reads like a stockout, and on its own it
	// is treated as one. But GKE folds three causes into that one message —
	// zone resource availability, cluster-wide maximums, and "no machine type
	// could fit the request" — and its only parameter is the zone. Here it is
	// the third: a 200-vCPU pod fits nothing, in any zone, ever.
	//
	// The discriminator is in the same record. NAP reports all four zones
	// exhausted while the two t2d-standard-8 MIGs that already exist reject the
	// pod on NodeResourcesFit. A zone that genuinely had no capacity would not
	// also be telling us the pod is too big for the shape. So a shape whose own
	// MIG says "the pod does not fit me" cannot have its zonal exhaustion taken
	// at face value.
	//
	// Without this, the demo's own provocation permanently condemns two healthy
	// zones: verified live, the ledger held both pairs at
	// nap.pod.zonal.resources.exceeded and us-central1 sat at 0.047.
	payload, err := os.ReadFile(filepath.Join("testdata", "provoke-both-lists.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ParseEntry(payload, time.Now()); len(got) != 0 {
		t.Fatalf("got %d observations from a pod that fits no shape, want 0: %+v", len(got), got)
	}
}

func TestParseEntryKeepsZonalExhaustionWhenThePredicateIsNotAboutSize(t *testing.T) {
	// Only NodeResourcesFit means "the pod is too big for this shape", and only
	// that reading makes zonal.resources.exceeded suspect. A pod excluded by
	// affinity or a taint says nothing about whether the zone had capacity, so
	// suppressing on any predicate at all would throw away real evidence.
	payload := `{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
	  "napFailureReasons":[{"messageId":"no.scale.up.nap.pod.zonal.resources.exceeded",
	    "parameters":["us-central1-b"]}],
	  "rejectedMigs":[{
	    "mig":{"name":"gke-spot-demo-nap-t2d-standard-8-x-grp","zone":"us-central1-b"},
	    "reason":{"messageId":"no.scale.up.mig.failing.predicate",
	      "parameters":["NodeAffinity","node(s) didn't match node selector"]}}]}]}}}`
	got := ParseEntry([]byte(payload), time.Now())
	if len(got) != 1 {
		t.Fatalf("got %d observations, want 1: %+v", len(got), got)
	}
	if got[0].Reason != "no.scale.up.nap.pod.zonal.resources.exceeded" {
		t.Errorf("Reason = %q, want the capacity reason", got[0].Reason)
	}
}

func TestParseEntrySuppressesOnlyTheShapeThePodDoesNotFit(t *testing.T) {
	// The suppression is per shape, not per pod group. A pod can be too big for
	// a t2d-standard-8 while a2-highgpu-1 in the same record is genuinely out
	// of capacity; dropping the whole group would lose the second.
	payload := `{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
	  "napFailureReasons":[{"messageId":"no.scale.up.nap.pod.zonal.resources.exceeded",
	    "parameters":["us-central1-b"]}],
	  "rejectedMigs":[
	    {"mig":{"name":"gke-spot-demo-nap-t2d-standard-8-x-grp","zone":"us-central1-b"},
	     "reason":{"messageId":"no.scale.up.mig.failing.predicate",
	       "parameters":["NodeResourcesFit","Insufficient cpu"]}},
	    {"mig":{"name":"gke-spot-demo-nap-a2-highgpu-1-y-grp","zone":"us-central1-c"}}]}]}}}`
	got := ParseEntry([]byte(payload), time.Now())
	if len(got) != 1 {
		t.Fatalf("got %d observations, want 1: %+v", len(got), got)
	}
	if got[0].MachineType != "a2-highgpu-1" {
		t.Errorf("MachineType = %q, want a2-highgpu-1 (t2d-standard-8 is suppressed)", got[0].MachineType)
	}
	if got[0].Zone != "us-central1-c" {
		t.Errorf("Zone = %q, want us-central1-c", got[0].Zone)
	}
}

func TestParseEntryStillReadsACapacityReasonAlongsideAPredicateRejection(t *testing.T) {
	// Filtering pod-fact reasons must not throw away the capacity reason
	// sitting next to one. The MIG's own reason is a predicate failure and is
	// dropped; the pod-group's zonal.resources.exceeded is real evidence, and
	// the MIG still supplies the shape and the authoritative zone.
	//
	// The zones deliberately disagree: the reason names -c, the MIG lives in
	// -b, and the observation lands in -b. A pod-group reason is not attached
	// to any MIG, so when the two disagree the MIG's own zone is the half of
	// the pairing that is actually attributable — its shape and its zone came
	// from the same object. Taking the reason's zone instead would pair a
	// shape with a zone where that shape was never tried.
	payload := `{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
	  "napFailureReasons":[
	    {"messageId":"no.scale.up.mig.skipped","parameters":["max cluster cpu limit reached"]},
	    {"messageId":"no.scale.up.nap.pod.zonal.resources.exceeded","parameters":["us-central1-c"]}],
	  "rejectedMigs":[{
	    "mig":{"name":"gke-spot-demo-nap-t2d-standard-8-spot-c553209c-grp","zone":"us-central1-b"},
	    "reason":{"messageId":"no.scale.up.mig.failing.predicate","parameters":["NodeAffinity"]}}]}]}}}`
	got := ParseEntry([]byte(payload), time.Now())
	if len(got) != 1 {
		t.Fatalf("got %d observations, want 1: %+v", len(got), got)
	}
	if got[0].Reason != "no.scale.up.nap.pod.zonal.resources.exceeded" {
		t.Errorf("Reason = %q, want the capacity reason", got[0].Reason)
	}
	if got[0].MachineType != "t2d-standard-8" {
		t.Errorf("MachineType = %q, want t2d-standard-8", got[0].MachineType)
	}
	if got[0].Zone != "us-central1-b" {
		t.Errorf("Zone = %q, want us-central1-b from the MIG", got[0].Zone)
	}
}

func TestParseEntryIgnoresMigsTheClusterOwns(t *testing.T) {
	// A pool whose name merely contains "nap" must not be read as a NAP MIG.
	// Real NAP names carry "-nap-" as a whole hyphen-delimited segment, so
	// "snap" fails that test while "gke-<cluster>-nap-..." passes it. Matching
	// a bare "nap-" substring would attribute a NAP failure to a shape NAP
	// never tried to create.
	for _, name := range []string{
		"gke-spot-demo-n2-standard-4-abc",
		"gke-spot-demo-snap-n2-standard-4-abc",
	} {
		payload := fmt.Sprintf(`{"noDecisionStatus":{"noScaleUp":{"unhandledPodGroups":[{
		  "napFailureReasons":[{"messageId":"no.scale.up.nap.pod.zonal.resources.exceeded",
		    "parameters":["us-central1-a"]}],
		  "rejectedMigs":[{"mig":{"name":%q,"zone":"us-central1-a"}}]}]}}}`, name)
		if got := ParseEntry([]byte(payload), time.Now()); len(got) != 0 {
			t.Errorf("mig %q: got %d observations, want 0: %+v", name, len(got), got)
		}
	}
}

func TestFilterIncludesScaleUpAndResults(t *testing.T) {
	f := Filter("p", "spot-demo", "us-central1", time.Unix(0, 0).UTC())
	for _, want := range []string{
		"jsonPayload.noDecisionStatus.noScaleUp:*",
		"jsonPayload.decision.scaleUp:*",
		"jsonPayload.resultInfo:*",
	} {
		if !strings.Contains(f, want) {
			t.Errorf("filter missing %q\n%s", want, f)
		}
	}
}

func TestFilterScopesToTheClusterAndTime(t *testing.T) {
	since := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	a := Filter("proj-a", "cluster-a", "us-central1", since)
	b := Filter("proj-b", "cluster-b", "europe-west4", since.Add(time.Hour))

	for _, want := range []string{
		`logName="projects/proj-a/logs/container.googleapis.com%2Fcluster-autoscaler-visibility"`,
		`resource.labels.cluster_name="cluster-a"`,
		`resource.labels.location="us-central1"`,
		`jsonPayload.noDecisionStatus.noScaleUp:*`,
		`timestamp>="2026-07-26T12:00:00Z"`,
	} {
		if !strings.Contains(a, want) {
			t.Errorf("filter missing %q:\n%s", want, a)
		}
	}
	// A filter that ignored its arguments would leak the other scope. The
	// cluster needles are quoted because the log name itself contains the
	// substring "cluster-a", in "cluster-autoscaler-visibility".
	for _, forbidden := range []string{"proj-b", `"cluster-b"`, "europe-west4", "13:00:00"} {
		if strings.Contains(a, forbidden) {
			t.Errorf("filter for cluster-a leaked %q:\n%s", forbidden, a)
		}
	}
	for _, forbidden := range []string{"proj-a", `"cluster-a"`, "us-central1", "12:00:00"} {
		if strings.Contains(b, forbidden) {
			t.Errorf("filter for cluster-b leaked %q:\n%s", forbidden, b)
		}
	}
}

func TestNewReportsAnUnreadableCredentialFile(t *testing.T) {
	// Fails during credential discovery, so it needs no network and no
	// credentials of its own.
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent/creds.json")
	r, err := New(context.Background(), "proj-a", "cluster-a", "us-central1")
	if err == nil {
		t.Fatalf("New succeeded with an unreadable credential file: %+v", r)
	}
	if r != nil {
		t.Errorf("New returned a Reader alongside an error: %+v", r)
	}
	if !strings.Contains(err.Error(), "logging client") {
		t.Errorf("error = %q, want it to name the failing step", err)
	}
}

func TestScaleUpFailures_HappyPath(t *testing.T) {
	decision := []byte(`{"decision":{"eventId":"e1","scaleUp":{"increasedMigs":[
	  {"mig":{"name":"gke-spot-demo-nap-g2-standard-4-spot-abc-grp","zone":"us-central1-a"}}]}}}`)
	result := []byte(`{"resultInfo":{"results":[
	  {"eventId":"e1","errorMsg":{"messageId":"scale.up.error.out.of.resources",
	   "parameters":["gke-spot-demo-nap-g2-standard-4-spot-abc-grp"]}}]}}`)
	ts := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	got := scaleUpFailures([]logEntry{{decision, ts}, {result, ts}})
	want := []evidence.Observation{{
		MachineType: "g2-standard-4", Zone: "us-central1-a",
		Reason: "scale.up.error.out.of.resources", At: ts,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestScaleUpFailures_OrphanResultDropped(t *testing.T) {
	// result references eventId with no decision in the page
	result := []byte(`{"resultInfo":{"results":[{"eventId":"missing",
	  "errorMsg":{"messageId":"scale.up.error.out.of.resources",
	  "parameters":["gke-spot-demo-nap-g2-standard-4-spot-abc-grp"]}}]}}`)
	if got := scaleUpFailures([]logEntry{{result, time.Now()}}); got != nil {
		t.Fatalf("orphan result must yield nothing, got %+v", got)
	}
}

func TestScaleUpFailures_PartialFailureOneZone(t *testing.T) {
	decision := []byte(`{"decision":{"eventId":"e1","scaleUp":{"increasedMigs":[
	  {"mig":{"name":"mig-a","zone":"us-central1-a"}},
	  {"mig":{"name":"mig-b","zone":"us-central1-b"}}]}}}`)
	// only mig-a (a g2-standard-4 NAP MIG) failed; use real NAP names
	decision = []byte(`{"decision":{"eventId":"e1","scaleUp":{"increasedMigs":[
	  {"mig":{"name":"gke-spot-demo-nap-g2-standard-4-spot-aaa-grp","zone":"us-central1-a"}},
	  {"mig":{"name":"gke-spot-demo-nap-g2-standard-4-spot-bbb-grp","zone":"us-central1-b"}}]}}}`)
	result := []byte(`{"resultInfo":{"results":[{"eventId":"e1",
	  "errorMsg":{"messageId":"scale.up.error.out.of.resources",
	  "parameters":["gke-spot-demo-nap-g2-standard-4-spot-aaa-grp"]}}]}}`)
	got := scaleUpFailures([]logEntry{{decision, time.Unix(0, 0)}, {result, time.Unix(0, 0)}})
	if len(got) != 1 || got[0].Zone != "us-central1-a" {
		t.Fatalf("want exactly one obs in us-central1-a, got %+v", got)
	}
}

func TestScaleUpFailures_NonStockoutIgnored(t *testing.T) {
	decision := []byte(`{"decision":{"eventId":"e1","scaleUp":{"increasedMigs":[
	  {"mig":{"name":"gke-spot-demo-nap-g2-standard-4-spot-abc-grp","zone":"us-central1-a"}}]}}}`)
	result := []byte(`{"resultInfo":{"results":[{"eventId":"e1",
	  "errorMsg":{"messageId":"scale.up.error.quota.exceeded",
	  "parameters":["gke-spot-demo-nap-g2-standard-4-spot-abc-grp"]}}]}}`)
	if got := scaleUpFailures([]logEntry{{decision, time.Unix(0, 0)}, {result, time.Unix(0, 0)}}); got != nil {
		t.Fatalf("quota error must be ignored, got %+v", got)
	}
}

func TestScaleUpFailures_UnparseableMigDropped(t *testing.T) {
	decision := []byte(`{"decision":{"eventId":"e1","scaleUp":{"increasedMigs":[
	  {"mig":{"name":"gke-spot-demo-default-pool-xyz-grp","zone":"us-central1-a"}}]}}}`)
	result := []byte(`{"resultInfo":{"results":[{"eventId":"e1",
	  "errorMsg":{"messageId":"scale.up.error.out.of.resources",
	  "parameters":["gke-spot-demo-default-pool-xyz-grp"]}}]}}`)
	if got := scaleUpFailures([]logEntry{{decision, time.Unix(0, 0)}, {result, time.Unix(0, 0)}}); got != nil {
		t.Fatalf("non-NAP MIG has no parseable shape; must drop, got %+v", got)
	}
}

// TestReaderMergesNoScaleUpAndScaleUp exercises the reader's real post-fetch
// path (merge) over one page holding both signals, and asserts the out-of-
// resources scale-up Observation survives. Calling merge — not ParseEntry and
// scaleUpFailures separately — is the point: this fails if the scaleUpFailures
// wiring is ever dropped from merge, which a boundary-only test could not catch.
// Matches the existing inline fixture pattern (os.ReadFile); there is no
// readFixture helper.
func TestReaderMergesNoScaleUpAndScaleUp(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ts := time.Unix(0, 0)
	// One page: a noScaleUp record (pod facts, may yield 0 obs) alongside the
	// scale-up decision and its out-of-resources result, split across entries
	// exactly as the Logging API delivers them.
	entries := []logEntry{
		{read("provoke-both-lists.json"), ts},
		{read("scaleup-stockout-decision.json"), ts},
		{read("scaleup-stockout-result.json"), ts},
	}
	got := merge(entries)

	want := evidence.Observation{
		MachineType: "g2-standard-4",
		Zone:        "us-central1-a",
		Reason:      "scale.up.error.out.of.resources",
		At:          ts,
	}
	var found bool
	for _, o := range got {
		if o == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("merge dropped the scaleUp stockout observation; got %+v, want it to contain %+v", got, want)
	}
}
