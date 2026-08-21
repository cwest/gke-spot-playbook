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

// Package work drains the tasks subscription, simulating per-task effort and
// emitting a completion receipt per task. Designed for spot nodes: Run returns
// promptly on ctx cancel (SIGTERM path) and never acks unfinished work, so
// preempted tasks are redelivered to a surviving replica.
package work

import (
	"context"
	"log"
	"strconv"
	"time"

	"cloud.google.com/go/pubsub"
)

type Worker struct {
	Sub         *pubsub.Subscription
	Completions *pubsub.Topic
}

// Run blocks until ctx is canceled. One message at a time per replica:
// horizontal scale comes from KEDA replicas, and small in-flight counts keep
// the SIGTERM drain inside the 15s spot grace window.
func (w *Worker) Run(ctx context.Context) error {
	w.Sub.ReceiveSettings.MaxOutstandingMessages = 1
	w.Sub.ReceiveSettings.NumGoroutines = 1
	return w.Sub.Receive(ctx, func(mctx context.Context, m *pubsub.Message) {
		id := m.Attributes["task_id"]
		sleepMS, err := strconv.Atoi(m.Attributes["sleep_ms"])
		if err != nil {
			sleepMS = 1000
		}
		select {
		case <-time.After(time.Duration(sleepMS) * time.Millisecond):
		case <-mctx.Done():
			m.Nack() // preemption mid-task: return it to the queue immediately
			return
		}
		res := w.Completions.Publish(mctx, &pubsub.Message{
			Data:       []byte(id),
			Attributes: map[string]string{"task_id": id},
		})
		if _, err := res.Get(mctx); err != nil {
			log.Printf("completion publish failed for %s (nacking): %v", id, err)
			m.Nack()
			return
		}
		m.Ack()
		log.Printf("done %s", id)
	})
}
