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

// Package gcp implements advice.API against the Compute Engine beta advice
// endpoints and the v1 regions list.
package gcp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computev1pb "cloud.google.com/go/compute/apiv1/computepb"
	computebeta "cloud.google.com/go/compute/apiv1beta"
	"cloud.google.com/go/compute/apiv1beta/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"

	"github.com/cwest/gke-spot-instance-node-pools/advisor/internal/advice"
)

type Client struct {
	adv     *computebeta.AdviceClient
	regions *compute.RegionsClient
}

func New(ctx context.Context, opts ...option.ClientOption) (*Client, error) {
	adv, err := computebeta.NewAdviceRESTClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	rc, err := compute.NewRegionsRESTClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{adv: adv, regions: rc}, nil
}

func (c *Client) Regions(ctx context.Context, project string) ([]string, error) {
	it := c.regions.List(ctx, &computev1pb.ListRegionsRequest{Project: project})
	var out []string
	for {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r.GetName())
	}
	return out, nil
}

func (c *Client) Capacity(ctx context.Context, q advice.CapacityQuery) ([]advice.CapacityResult, error) {
	sel := &computepb.CapacityAdviceRequestInstanceFlexibilityPolicyInstanceSelection{
		MachineTypes: q.MachineTypes,
	}
	req := &computepb.CapacityAdviceRpcRequest{
		Project: q.Project,
		Region:  q.Region,
		CapacityAdviceRequestResource: &computepb.CapacityAdviceRequest{
			InstanceProperties: &computepb.CapacityAdviceRequestInstanceProperties{
				Scheduling: &computepb.CapacityAdviceRequestInstancePropertiesScheduling{
					ProvisioningModel: proto.String("SPOT"),
				},
			},
			InstanceFlexibilityPolicy: &computepb.CapacityAdviceRequestInstanceFlexibilityPolicy{
				InstanceSelections: map[string]*computepb.CapacityAdviceRequestInstanceFlexibilityPolicyInstanceSelection{
					"selection-1": sel,
				},
			},
			Size: proto.Int32(q.Size),
			DistributionPolicy: &computepb.CapacityAdviceRequestDistributionPolicy{
				TargetShape: proto.String("ANY"),
			},
		},
	}
	resp, err := c.adv.Capacity(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("advice.capacity %s/%s: %w", q.Region, strings.Join(q.MachineTypes, ","), err)
	}
	var out []advice.CapacityResult
	for _, rec := range resp.GetRecommendations() {
		r := advice.CapacityResult{
			Obtainability:          rec.GetScores().GetObtainability(),
			EstimatedUptimeSeconds: parseSeconds(rec.GetScores().GetEstimatedUptime()),
		}
		for _, s := range rec.GetShards() {
			r.Shards = append(r.Shards, advice.Shard{
				Zone:        lastSegment(s.GetZone()),
				MachineType: s.GetMachineType(),
				Count:       s.GetInstanceCount(),
			})
		}
		out = append(out, r)
	}
	return out, nil
}

func (c *Client) CapacityHistory(ctx context.Context, q advice.HistoryQuery) (*advice.HistoryResult, error) {
	req := &computepb.CapacityHistoryAdviceRequest{
		Project: q.Project,
		Region:  q.Region,
		CapacityHistoryRequestResource: &computepb.CapacityHistoryRequest{
			InstanceProperties: &computepb.CapacityHistoryRequestInstanceProperties{
				MachineType: proto.String(q.MachineType),
				Scheduling: &computepb.CapacityHistoryRequestInstancePropertiesScheduling{
					ProvisioningModel: proto.String("SPOT"),
				},
			},
			Types: []string{"PREEMPTION", "PRICE"},
		},
	}
	// A zone pins the query to one zone; an empty zone requests region-aggregated
	// history, which the API expects expressed by omitting locationPolicy entirely.
	if q.Zone != "" {
		req.CapacityHistoryRequestResource.LocationPolicy = &computepb.CapacityHistoryRequestLocationPolicy{
			Location: proto.String("zones/" + q.Zone),
		}
	}
	resp, err := c.adv.CapacityHistory(ctx, req)
	if err != nil {
		loc := q.Zone
		if loc == "" {
			loc = q.Region
		}
		return nil, fmt.Errorf("advice.capacityHistory %s/%s: %w", loc, q.MachineType, err)
	}
	out := &advice.HistoryResult{}

	// The API contract does not guarantee ordering, but advice.HistoryResult
	// promises DailyPreemptionRates oldest-first. Sort ascending by interval
	// start time before extracting rates.
	preempt := append([]*computepb.CapacityHistoryResponsePreemptionRecord(nil), resp.GetPreemptionHistory()...)
	sort.SliceStable(preempt, func(i, j int) bool {
		return intervalStart(preempt[i].GetInterval().GetStartTime()).Before(
			intervalStart(preempt[j].GetInterval().GetStartTime()))
	})
	for _, p := range preempt {
		out.DailyPreemptionRates = append(out.DailyPreemptionRates, p.GetPreemptionRate())
	}

	// Choose the price entry with the max interval start time as "latest",
	// rather than trusting array order.
	var latest *computepb.CapacityHistoryResponsePriceRecord
	for _, p := range resp.GetPriceHistory() {
		if latest == nil || intervalStart(p.GetInterval().GetStartTime()).After(
			intervalStart(latest.GetInterval().GetStartTime())) {
			latest = p
		}
	}
	if latest != nil {
		out.LatestSpotUSDPerHour = float64(latest.GetListPrice().GetUnits()) +
			float64(latest.GetListPrice().GetNanos())/1e9
	}
	return out, nil
}

// intervalStart parses an RFC3339 interval start time. Unparseable values sort
// as the zero time (oldest), keeping them stable at the front.
func intervalStart(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseSeconds(d string) int {
	n, err := strconv.Atoi(strings.TrimSuffix(d, "s"))
	if err != nil {
		return 0
	}
	return n
}

func lastSegment(url string) string {
	parts := strings.Split(url, "/")
	return parts[len(parts)-1]
}
