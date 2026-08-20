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

// agent: Act 5 idle-bursty mock agent with resident working set.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	var (
		wsSize        = flag.Int("working-set-mib", 256, "working set size in MiB")
		activeSeconds = flag.Int("active-seconds", 10, "active phase duration in seconds")
		idleSeconds   = flag.Int("idle-seconds", 30, "idle phase duration in seconds")
		stateFile     = flag.String("state-file", "", "file to write phase transitions")
	)
	flag.Parse()

	if *wsSize <= 0 {
		log.Fatalf("working-set-mib must be > 0, got %d", *wsSize)
	}

	ws := NewWorkingSet(*wsSize)
	active := time.Duration(*activeSeconds) * time.Second
	idle := time.Duration(*idleSeconds) * time.Second
	cycle := active + idle

	log.Printf("agent starting (working-set=%dMiB, active=%v, idle=%v)", *wsSize, active, idle)

	lastPhase := -1
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			log.Printf("agent interrupted, exiting")
			return
		case <-tick.C:
			elapsed := time.Since(start)
			phase := phaseAt(elapsed, active, idle)

			if int(phase) != lastPhase {
				lastPhase = int(phase)
				phaseStr := "active"
				if phase == Idle {
					phaseStr = "idle"
				}
				log.Printf("phase transition: %s (elapsed=%v, cycle_pos=%v)", phaseStr, elapsed, elapsed%cycle)
				if *stateFile != "" {
					if err := os.WriteFile(*stateFile, []byte(phaseStr), 0o644); err != nil {
						log.Printf("error writing state file: %v", err)
					}
				}
			}

			if ws == nil {
				log.Fatalf("internal error: working set is nil")
			}
		}
	}
}
