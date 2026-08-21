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
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"google.golang.org/api/option"
)

// expectedT2D8FromFixture is the composed on-demand list price for
// t2d-standard-8 (8 vCPU, 32 GiB) using the REAL us-south1 prices copied into
// advisor/testdata/billing_skus.json:
//
//	core "T2D AMD Instance Core running in Dallas" OnDemand: units=0 nanos=32452360 -> $0.03245236 /vCPU/h
//	ram  "T2D AMD Instance Ram running in Dallas"  OnDemand: units=0 nanos=4349480  -> $0.00434948 /GiB/h
//
//	8 * 0.03245236  = 0.25961888
//	32 * 0.00434948 = 0.13918336
//	                  ----------
//	sum             = 0.39880224
const expectedT2D8FromFixture = 0.39880224

func billingServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile("../../testdata/billing_skus.json")
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
}

func TestOnDemandHourlyUSDComposesCoreAndRam(t *testing.T) {
	srv := billingServer(t)
	defer srv.Close()
	src, err := NewBillingSource(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.OnDemandHourlyUSD(context.Background(), "us-south1", "t2d-standard-8")
	if err != nil {
		t.Fatal(err)
	}
	want := expectedT2D8FromFixture
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("t2d-standard-8 = %v, want %v", got, want)
	}
}

func TestOnDemandHourlyUSDG2(t *testing.T) {
	srv := billingServer(t)
	defer srv.Close()
	src, err := NewBillingSource(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.OnDemandHourlyUSD(context.Background(), "us-central1", "g2-standard-8")
	if err != nil {
		t.Fatalf("g2-standard-8: %v", err)
	}
	// 8*0.024988212 + 32*0.002927448 + 1*0.560040239 (fixture = live catalog 2026-07-24)
	want := 0.853624
	if math.Abs(got-want) > 1e-4 {
		t.Fatalf("g2-standard-8 = %v, want ≈%v — check the DWS decoy SKU is excluded", got, want)
	}
}

func TestOnDemandUnsupportedFamily(t *testing.T) {
	srv := billingServer(t)
	defer srv.Close()
	src, _ := NewBillingSource(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	// n1 is not in familyMarkers, so it stays unsupported after g2 is priced.
	if _, err := src.OnDemandHourlyUSD(context.Background(), "us-south1", "n1-standard-8"); err == nil {
		t.Error("want error for unsupported family")
	}
}
