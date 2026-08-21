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

// publisher seeds the Act 1 task queue. Run locally:
//
//	go run ./cmd/publisher --project example-sandbox --count 5000 --sleep-ms 1500
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strconv"

	"cloud.google.com/go/pubsub"
)

func main() {
	project := flag.String("project", "example-sandbox", "GCP project")
	topicID := flag.String("topic", "spot-demo-tasks", "tasks topic")
	count := flag.Int("count", 5000, "number of tasks")
	sleepMS := flag.Int("sleep-ms", 1500, "simulated per-task work duration")
	flag.Parse()

	ctx := context.Background()
	client, err := pubsub.NewClient(ctx, *project)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()
	topic := client.Topic(*topicID)
	defer topic.Stop()

	var results []*pubsub.PublishResult
	for i := 0; i < *count; i++ {
		results = append(results, topic.Publish(ctx, &pubsub.Message{
			Data: []byte("work"),
			Attributes: map[string]string{
				"task_id":  fmt.Sprintf("task-%06d", i),
				"sleep_ms": strconv.Itoa(*sleepMS),
			},
		}))
	}
	for i, r := range results {
		if _, err := r.Get(ctx); err != nil {
			log.Fatalf("publish %d: %v", i, err)
		}
	}
	fmt.Printf("published %d tasks to %s\n", *count, *topicID)
}
