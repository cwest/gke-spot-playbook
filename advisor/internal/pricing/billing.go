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

package pricing

import (
	"context"
	"fmt"
	"strings"
	"sync"

	cloudbilling "google.golang.org/api/cloudbilling/v1"
	"google.golang.org/api/option"
)

// computeServiceID is the Cloud Billing Catalog service ID for Compute Engine.
const computeServiceID = "services/6F81-5844-456A"

// familyMarkers maps a machine family to the substrings that identify its
// on-demand per-core and per-GiB SKU descriptions in the Billing Catalog.
// Recorded from the live us-south1 catalog (region renders as "Dallas"):
//
//	t2d -> "T2D AMD Instance Core running in Dallas" / "... Ram ..."
//	e2  -> "E2 Instance Core running in Dallas"      / "... Ram ..."
//	n2  -> "N2 Instance Core running in Dallas"      / "... Ram ..."
//	g2  -> "G2 Instance Core running in Americas"    / "... Ram ..."
//
// The markers deliberately omit the region so they match every region, and are
// specific enough to exclude Custom/Sole Tenancy/Spot variants (those carry
// extra words or a different usageType).
var familyMarkers = map[string]struct{ core, ram string }{
	"t2d": {core: "T2D AMD Instance Core", ram: "T2D AMD Instance Ram"},
	"e2":  {core: "E2 Instance Core", ram: "E2 Instance Ram"},
	"n2":  {core: "N2 Instance Core", ram: "N2 Instance Ram"},
	"g2":  {core: "G2 Instance Core", ram: "G2 Instance Ram"},
}

// gpuMarkers maps a family to the SKU-description marker of its bundled GPU.
// Only families with attached accelerators appear here.
var gpuMarkers = map[string]string{"g2": "Nvidia L4 GPU"}

// excludedMarkers are description substrings that disqualify a SKU even when it
// matches a family marker (variant SKUs we never price against). "DWS" excludes
// "Nvidia L4 GPU attached to DWS Defined Duration VMs ...", which is also
// usageType OnDemand and would otherwise double-match the L4 GPU marker.
var excludedMarkers = []string{"Sole Tenancy", "Custom", "Commitment", "DWS"}

// BillingSource resolves on-demand list prices from the Cloud Billing Catalog.
type BillingSource struct {
	svc *cloudbilling.APIService

	mu    sync.Mutex
	cache map[string]rates // key: region + "/" + family
}

// rates holds the resolved per-unit prices for one region+family. gpu is 0 for
// pure-CPU families (no gpuMarkers entry).
type rates struct {
	core, ram, gpu float64
}

// NewBillingSource wraps the Cloud Billing Catalog API.
func NewBillingSource(ctx context.Context, opts ...option.ClientOption) (*BillingSource, error) {
	svc, err := cloudbilling.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &BillingSource{svc: svc, cache: map[string]rates{}}, nil
}

// OnDemandHourlyUSD composes the machine type's list price from its per-core,
// per-GiB, and (for GPU families) per-GPU Compute SKUs for the region.
func (b *BillingSource) OnDemandHourlyUSD(ctx context.Context, region, machineType string) (float64, error) {
	shape, err := ParseShape(machineType)
	if err != nil {
		return 0, err
	}
	p, err := b.resolve(ctx, region, shape.Family)
	if err != nil {
		return 0, err
	}
	return shape.VCPUs*p.core + shape.MemoryGB*p.ram + shape.GPUs*p.gpu, nil
}

// resolve returns the per-core and per-GiB prices for region+family, listing the
// catalog once and caching the result so repeated calls don't re-list.
func (b *BillingSource) resolve(ctx context.Context, region, family string) (rates, error) {
	markers, ok := familyMarkers[family]
	if !ok {
		return rates{}, fmt.Errorf("%w: %s", ErrUnsupported, family)
	}
	gpuMarker, wantGPU := gpuMarkers[family]
	key := region + "/" + family
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.cache[key]; ok {
		return p, nil
	}

	var (
		corePrice, ramPrice, gpuPrice float64
		foundCore, foundRam, foundGPU bool
	)
	err := b.svc.Services.Skus.List(computeServiceID).PageSize(5000).Pages(ctx,
		func(page *cloudbilling.ListSkusResponse) error {
			for _, sku := range page.Skus {
				if sku.Category == nil ||
					sku.Category.ResourceFamily != "Compute" ||
					sku.Category.UsageType != "OnDemand" {
					continue
				}
				if !containsRegion(sku.ServiceRegions, region) {
					continue
				}
				desc := sku.Description
				if hasAny(desc, excludedMarkers) {
					continue
				}
				switch {
				case !foundCore && strings.Contains(desc, markers.core):
					price, err := unitPrice(sku)
					if err != nil {
						return err
					}
					corePrice, foundCore = price, true
				case !foundRam && strings.Contains(desc, markers.ram):
					price, err := unitPrice(sku)
					if err != nil {
						return err
					}
					ramPrice, foundRam = price, true
				case wantGPU && !foundGPU && strings.Contains(desc, gpuMarker):
					price, err := unitPrice(sku)
					if err != nil {
						return err
					}
					gpuPrice, foundGPU = price, true
				}
			}
			return nil
		})
	if err != nil {
		return rates{}, fmt.Errorf("list compute skus: %w", err)
	}
	if !foundCore {
		return rates{}, fmt.Errorf("no on-demand core SKU for %s in %s", family, region)
	}
	if !foundRam {
		return rates{}, fmt.Errorf("no on-demand ram SKU for %s in %s", family, region)
	}
	if wantGPU && !foundGPU {
		return rates{}, fmt.Errorf("no on-demand GPU SKU (marker %q) for %s in %s", gpuMarker, family, region)
	}
	p := rates{core: corePrice, ram: ramPrice, gpu: gpuPrice}
	b.cache[key] = p
	return p, nil
}

// unitPrice extracts the first tier's unit price from a SKU as USD.
func unitPrice(sku *cloudbilling.Sku) (float64, error) {
	if len(sku.PricingInfo) == 0 ||
		sku.PricingInfo[0].PricingExpression == nil ||
		len(sku.PricingInfo[0].PricingExpression.TieredRates) == 0 {
		return 0, fmt.Errorf("sku %s has no tiered rates", sku.SkuId)
	}
	up := sku.PricingInfo[0].PricingExpression.TieredRates[0].UnitPrice
	if up == nil {
		return 0, fmt.Errorf("sku %s has no unit price", sku.SkuId)
	}
	return float64(up.Units) + float64(up.Nanos)/1e9, nil
}

func containsRegion(regions []string, region string) bool {
	for _, r := range regions {
		if r == region {
			return true
		}
	}
	return false
}

func hasAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
