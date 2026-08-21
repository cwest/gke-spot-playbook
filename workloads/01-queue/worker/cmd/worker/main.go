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

// queue-worker: Act 1 spot-node task consumer.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"cloud.google.com/go/pubsub"

	"github.com/cwest/gke-spot-instance-node-pools/workloads/01-queue/worker/internal/work"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	project := env("PROJECT", "example-sandbox")
	client, err := pubsub.NewClient(ctx, project)
	if err != nil {
		log.Fatalf("pubsub client: %v", err)
	}
	defer client.Close()

	w := &work.Worker{
		Sub:         client.Subscription(env("TASKS_SUB", "spot-demo-tasks-sub")),
		Completions: client.Topic(env("COMPLETIONS_TOPIC", "spot-demo-completions")),
	}
	defer w.Completions.Stop()
	log.Printf("queue-worker starting (project=%s)", project)
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("receive: %v", err)
	}
	log.Printf("queue-worker drained, exiting")
}
