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

package work

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// harness spins up an in-memory Pub/Sub with tasks + completions plumbing.
type harness struct {
	client           *pubsub.Client
	tasksTopic       *pubsub.Topic
	tasksSub         *pubsub.Subscription
	completionsTopic *pubsub.Topic
	completionsSub   *pubsub.Subscription
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	srv := pstest.NewServer()
	t.Cleanup(func() { srv.Close() })
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	client, err := pubsub.NewClient(ctx, "p", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	tt, err := client.CreateTopic(ctx, "tasks")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := client.CreateSubscription(ctx, "tasks-sub", pubsub.SubscriptionConfig{Topic: tt, AckDeadline: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ct, err := client.CreateTopic(ctx, "completions")
	if err != nil {
		t.Fatal(err)
	}
	cs, err := client.CreateSubscription(ctx, "completions-sub", pubsub.SubscriptionConfig{Topic: ct})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{client, tt, ts, ct, cs}
}

func (h *harness) publishTasks(t *testing.T, n int, sleepMS string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		r := h.tasksTopic.Publish(ctx, &pubsub.Message{
			Data:       []byte("work"),
			Attributes: map[string]string{"task_id": fmt.Sprintf("task-%06d", i), "sleep_ms": sleepMS},
		})
		if _, err := r.Get(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *harness) collectCompletions(t *testing.T, want int, timeout time.Duration) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var mu sync.Mutex
	got := map[string]int{}
	_ = h.completionsSub.Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		mu.Lock()
		got[m.Attributes["task_id"]]++
		if len(got) >= want {
			cancel()
		}
		mu.Unlock()
		m.Ack()
	})
	return got
}

func TestRunProcessesAllTasksAndPublishesCompletions(t *testing.T) {
	h := newHarness(t)
	h.publishTasks(t, 5, "10")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	w := &Worker{Sub: h.tasksSub, Completions: h.completionsTopic}
	go func() {
		time.Sleep(2 * time.Second) // let it drain, then stop like a SIGTERM would
		cancel()
	}()
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
	got := h.collectCompletions(t, 5, 5*time.Second)
	if len(got) != 5 {
		t.Fatalf("distinct completions = %d, want 5 (%v)", len(got), got)
	}
}

func TestRunStopsPromptlyOnCancelWithoutLosingTasks(t *testing.T) {
	h := newHarness(t)
	h.publishTasks(t, 3, "300")
	w := &Worker{Sub: h.tasksSub, Completions: h.completionsTopic}

	// First run: cancel almost immediately mid-work (simulates SIGTERM).
	ctx1, cancel1 := context.WithCancel(context.Background())
	go func() { time.Sleep(400 * time.Millisecond); cancel1() }()
	start := time.Now()
	_ = w.Run(ctx1)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run did not stop promptly after cancel: %v", elapsed)
	}

	// Second run (fresh worker = replacement pod): everything still completes,
	// proving unacked tasks were redelivered, at-least-once end to end. Collect
	// completions concurrently and stop the worker the moment all three arrive,
	// rather than racing a fixed window against Pub/Sub's ack-deadline redelivery
	// (up to a full 10s AckDeadline cycle when run-1's nack doesn't propagate
	// before shutdown) — that race is what made this test flaky.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	runDone := make(chan struct{})
	go func() { _ = w.Run(ctx2); close(runDone) }()

	got := h.collectCompletions(t, 3, 25*time.Second)
	cancel2()
	<-runDone
	if len(got) != 3 {
		t.Fatalf("after restart, distinct completions = %d, want 3 (%v)", len(got), got)
	}
}
