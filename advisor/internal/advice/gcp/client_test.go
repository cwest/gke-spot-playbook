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

package gcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"google.golang.org/api/option"

	"github.com/cwest/gke-spot-playbook/advisor/internal/advice"
)

func server(t *testing.T) *httptest.Server {
	t.Helper()
	return serverWithHistory(t, "../../../testdata/history_response.json")
}

func serverWithHistory(t *testing.T, historyFile string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var file string
		switch {
		case strings.Contains(r.URL.Path, "/advice/capacity") && !strings.Contains(r.URL.Path, "History"):
			file = "../../../testdata/capacity_response.json"
		case strings.Contains(r.URL.Path, "/advice/capacityHistory"):
			file = historyFile
		default:
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
}

func newClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(context.Background(),
		option.WithEndpoint(url), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCapacityNormalizesResponse(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	got, err := newClient(t, srv.URL).Capacity(context.Background(), advice.CapacityQuery{
		Project: "p", Region: "us-central1",
		MachineTypes: []string{"n2-standard-2", "n2-standard-4"}, Size: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Obtainability != 0.9 || got[0].EstimatedUptimeSeconds != 600 {
		t.Fatalf("scores: %+v", got)
	}
	if len(got[0].Shards) != 2 || got[0].Shards[0].Zone != "us-central1-a" || got[0].Shards[0].Count != 90 {
		t.Fatalf("shards: %+v", got[0].Shards)
	}
}

func TestCapacityHistoryNormalizesResponse(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	got, err := newClient(t, srv.URL).CapacityHistory(context.Background(), advice.HistoryQuery{
		Project: "p", Region: "us-central1", Zone: "us-central1-a", MachineType: "n2-standard-32",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0.52, 0.31}
	if len(got.DailyPreemptionRates) != 2 || got.DailyPreemptionRates[0] != want[0] {
		t.Fatalf("rates: %+v", got.DailyPreemptionRates)
	}
	if got.LatestSpotUSDPerHour != 0.47872 {
		t.Fatalf("price: %v", got.LatestSpotUSDPerHour)
	}
}

// captureServer records the last request body it received, then replies with the
// standard history fixture so the client can finish parsing.
func captureServer(t *testing.T, body *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/advice/capacityHistory") {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			*body = string(b)
		}
		f, err := os.ReadFile("../../../testdata/history_response.json")
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(f)
	}))
}

// A region-aggregated history query (empty Zone) must omit the locationPolicy
// block entirely; a zone-scoped query must include it. The positive case guards
// against the negative assertion passing merely because the field name changed.
func TestCapacityHistoryOmitsLocationForEmptyZone(t *testing.T) {
	var body string
	srv := captureServer(t, &body)
	defer srv.Close()
	c := newClient(t, srv.URL)

	if _, err := c.CapacityHistory(context.Background(), advice.HistoryQuery{
		Project: "p", Region: "us-central1", MachineType: "n2-standard-32",
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "locationPolicy") {
		t.Errorf("empty-zone request must omit locationPolicy, got body:\n%s", body)
	}

	if _, err := c.CapacityHistory(context.Background(), advice.HistoryQuery{
		Project: "p", Region: "us-central1", Zone: "us-central1-a", MachineType: "n2-standard-32",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "locationPolicy") {
		t.Errorf("zone-scoped request must include locationPolicy, got body:\n%s", body)
	}
}

func TestCapacityHistorySortsByIntervalStart(t *testing.T) {
	srv := serverWithHistory(t, "../../../testdata/history_response_unordered.json")
	defer srv.Close()
	got, err := newClient(t, srv.URL).CapacityHistory(context.Background(), advice.HistoryQuery{
		Project: "p", Region: "us-central1", Zone: "us-central1-a", MachineType: "n2-standard-32",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Despite the shuffled fixture, rates must be normalized oldest-first and
	// the price with the max interval start must win (not the last array entry).
	want := []float64{0.52, 0.31}
	if len(got.DailyPreemptionRates) != 2 ||
		got.DailyPreemptionRates[0] != want[0] || got.DailyPreemptionRates[1] != want[1] {
		t.Fatalf("rates: %+v", got.DailyPreemptionRates)
	}
	if got.LatestSpotUSDPerHour != 0.47872 {
		t.Fatalf("price: %v", got.LatestSpotUSDPerHour)
	}
}
