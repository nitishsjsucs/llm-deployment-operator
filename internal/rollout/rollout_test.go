/*
Copyright 2026 The llm-deployment-operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rollout

import (
	"slices"
	"testing"
	"time"
)

var (
	t0  = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cfg = Config{Strategy: Canary, Steps: []int32{10, 50, 100}, StepInterval: 5 * time.Minute, ProgressDeadline: 30 * time.Minute}
)

func TestNext(t *testing.T) {
	tests := []struct {
		name       string
		cur        State
		cfg        Config
		obs        Observation
		wantAction Action
		wantNext   State
		wantWeight int32
		wantRequeu time.Duration
	}{
		{
			name:       "first revision becomes stable directly",
			cur:        State{},
			obs:        Observation{Desired: "a", Now: t0},
			wantAction: ActionInitialize,
			wantNext:   State{Stable: "a"},
		},
		{
			name:       "steady state",
			cur:        State{Stable: "a"},
			obs:        Observation{Desired: "a", Now: t0},
			wantAction: ActionNone,
			wantNext:   State{Stable: "a"},
		},
		{
			name:       "new revision starts a warming canary",
			cur:        State{Stable: "a"},
			obs:        Observation{Desired: "b", Now: t0},
			wantAction: ActionStartCanary,
			wantNext:   State{Stable: "a", Canary: "b", Step: WarmingStep, StepStarted: t0},
			wantRequeu: 30 * time.Minute,
		},
		{
			name:       "warming canary waits for readiness with no traffic",
			cur:        State{Stable: "a", Canary: "b", Step: WarmingStep, StepStarted: t0},
			obs:        Observation{Desired: "b", Now: t0.Add(10 * time.Minute)},
			wantAction: ActionWait,
			wantNext:   State{Stable: "a", Canary: "b", Step: WarmingStep, StepStarted: t0},
			wantRequeu: 20 * time.Minute,
		},
		{
			name:       "ready warming canary moves to the first step",
			cur:        State{Stable: "a", Canary: "b", Step: WarmingStep, StepStarted: t0},
			obs:        Observation{Desired: "b", CanaryReady: true, Now: t0.Add(3 * time.Minute)},
			wantAction: ActionAdvance,
			wantNext:   State{Stable: "a", Canary: "b", Step: 0, StepStarted: t0.Add(3 * time.Minute)},
			wantWeight: 10,
			wantRequeu: 5 * time.Minute,
		},
		{
			name:       "step holds until the interval passes",
			cur:        State{Stable: "a", Canary: "b", Step: 0, StepStarted: t0},
			obs:        Observation{Desired: "b", CanaryReady: true, Now: t0.Add(2 * time.Minute)},
			wantAction: ActionWait,
			wantNext:   State{Stable: "a", Canary: "b", Step: 0, StepStarted: t0},
			wantWeight: 10,
			wantRequeu: 3 * time.Minute,
		},
		{
			name:       "step advances after the interval",
			cur:        State{Stable: "a", Canary: "b", Step: 0, StepStarted: t0},
			obs:        Observation{Desired: "b", CanaryReady: true, Now: t0.Add(5 * time.Minute)},
			wantAction: ActionAdvance,
			wantNext:   State{Stable: "a", Canary: "b", Step: 1, StepStarted: t0.Add(5 * time.Minute)},
			wantWeight: 50,
			wantRequeu: 5 * time.Minute,
		},
		{
			name:       "last step promotes after the interval",
			cur:        State{Stable: "a", Canary: "b", Step: 2, StepStarted: t0},
			obs:        Observation{Desired: "b", CanaryReady: true, Now: t0.Add(5 * time.Minute)},
			wantAction: ActionPromote,
			wantNext:   State{Stable: "b"},
		},
		{
			name:       "unready canary loses its traffic but keeps its step",
			cur:        State{Stable: "a", Canary: "b", Step: 1, StepStarted: t0},
			obs:        Observation{Desired: "b", Now: t0.Add(10 * time.Minute)},
			wantAction: ActionWait,
			wantNext:   State{Stable: "a", Canary: "b", Step: 1, StepStarted: t0},
			wantRequeu: 20 * time.Minute,
		},
		{
			name:       "canary not ready by the deadline is rolled back",
			cur:        State{Stable: "a", Canary: "b", Step: WarmingStep, StepStarted: t0},
			obs:        Observation{Desired: "b", Now: t0.Add(30 * time.Minute)},
			wantAction: ActionRollback,
			wantNext:   State{Stable: "a", RolledBack: "b"},
		},
		{
			name:       "rolled back revision is not retried",
			cur:        State{Stable: "a", RolledBack: "b"},
			obs:        Observation{Desired: "b", Now: t0.Add(time.Hour)},
			wantAction: ActionHold,
			wantNext:   State{Stable: "a", RolledBack: "b"},
		},
		{
			name:       "a newer revision after a rollback starts a new canary",
			cur:        State{Stable: "a", RolledBack: "b"},
			obs:        Observation{Desired: "c", Now: t0},
			wantAction: ActionStartCanary,
			wantNext:   State{Stable: "a", Canary: "c", Step: WarmingStep, StepStarted: t0},
			wantRequeu: 30 * time.Minute,
		},
		{
			name:       "a newer revision replaces an in-flight canary",
			cur:        State{Stable: "a", Canary: "b", Step: 1, StepStarted: t0},
			obs:        Observation{Desired: "c", CanaryReady: true, Now: t0.Add(time.Minute)},
			wantAction: ActionStartCanary,
			wantNext:   State{Stable: "a", Canary: "c", Step: WarmingStep, StepStarted: t0.Add(time.Minute)},
			wantRequeu: 30 * time.Minute,
		},
		{
			name:       "reverting the spec aborts the canary",
			cur:        State{Stable: "a", Canary: "b", Step: 1, StepStarted: t0},
			obs:        Observation{Desired: "a", Now: t0},
			wantAction: ActionAbort,
			wantNext:   State{Stable: "a"},
		},
		{
			name:       "recreate swaps the stable revision",
			cur:        State{Stable: "a"},
			cfg:        Config{Strategy: Recreate, Steps: []int32{100}},
			obs:        Observation{Desired: "b", Now: t0},
			wantAction: ActionRecreate,
			wantNext:   State{Stable: "b"},
		},
		{
			name:       "steps shortened mid-rollout clamp to the last step",
			cur:        State{Stable: "a", Canary: "b", Step: 5, StepStarted: t0},
			obs:        Observation{Desired: "b", CanaryReady: true, Now: t0.Add(time.Minute)},
			wantAction: ActionWait,
			wantNext:   State{Stable: "a", Canary: "b", Step: 2, StepStarted: t0},
			wantWeight: 100,
			wantRequeu: 4 * time.Minute,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cfg
			if tt.cfg.Strategy != "" {
				c = tt.cfg
			}
			d := Next(tt.cur, c, tt.obs)
			if d.Action != tt.wantAction {
				t.Errorf("action = %s, want %s (%s)", d.Action, tt.wantAction, d.Message)
			}
			if d.Next != tt.wantNext {
				t.Errorf("next = %+v, want %+v", d.Next, tt.wantNext)
			}
			if w := Weight(d.Next, c.Steps, tt.obs.CanaryReady); w != tt.wantWeight {
				t.Errorf("weight = %d, want %d", w, tt.wantWeight)
			}
			if d.RequeueAfter != tt.wantRequeu {
				t.Errorf("requeue = %s, want %s", d.RequeueAfter, tt.wantRequeu)
			}
		})
	}
}

// TestFullCanaryProgression drives the state machine the way the controller
// does and records the weights it walks through.
func TestFullCanaryProgression(t *testing.T) {
	s := State{Stable: "a"}
	now := t0
	var weights []int32
	var actions []Action
	for i := 0; i < 20 && s.Stable != "b"; i++ {
		ready := now.Sub(t0) >= 2*time.Minute // the canary takes two minutes to load
		d := Next(s, cfg, Observation{Desired: "b", CanaryReady: ready, Now: now})
		actions = append(actions, d.Action)
		if d.Action == ActionAdvance {
			weights = append(weights, Weight(d.Next, cfg.Steps, ready))
		}
		s = d.Next
		now = now.Add(time.Minute)
	}
	if s.Stable != "b" || s.Canary != "" {
		t.Fatalf("not promoted, state %+v, actions %v", s, actions)
	}
	if want := []int32{10, 50, 100}; !slices.Equal(weights, want) {
		t.Errorf("weights = %v, want %v", weights, want)
	}
	if last := actions[len(actions)-1]; last != ActionPromote {
		t.Errorf("last action = %s, want Promote", last)
	}
}

func TestValidateSteps(t *testing.T) {
	tests := []struct {
		steps []int32
		ok    bool
	}{
		{[]int32{10, 50, 100}, true},
		{[]int32{100}, true},
		{nil, false},
		{[]int32{10, 50}, false},
		{[]int32{50, 10, 100}, false},
		{[]int32{10, 10, 100}, false},
		{[]int32{0, 100}, false},
		{[]int32{10, 150}, false},
	}
	for _, tt := range tests {
		if err := ValidateSteps(tt.steps); (err == nil) != tt.ok {
			t.Errorf("ValidateSteps(%v) = %v, want ok=%v", tt.steps, err, tt.ok)
		}
	}
}

func TestWeight(t *testing.T) {
	steps := []int32{10, 50, 100}
	tests := []struct {
		name  string
		s     State
		ready bool
		want  int32
	}{
		{"no canary", State{Stable: "a"}, true, 0},
		{"warming", State{Stable: "a", Canary: "b", Step: WarmingStep}, true, 0},
		{"not ready", State{Stable: "a", Canary: "b", Step: 1}, false, 0},
		{"ready at step 1", State{Stable: "a", Canary: "b", Step: 1}, true, 50},
	}
	for _, tt := range tests {
		if got := Weight(tt.s, steps, tt.ready); got != tt.want {
			t.Errorf("%s: Weight = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestStepWeight(t *testing.T) {
	steps := []int32{10, 50, 100}
	for step, want := range map[int]int32{WarmingStep: 10, 0: 10, 1: 50, 2: 100, 7: 100} {
		if got := StepWeight(steps, step); got != want {
			t.Errorf("StepWeight(%d) = %d, want %d", step, got, want)
		}
	}
}
