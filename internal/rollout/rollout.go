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

// Package rollout is the pure state machine behind canary and recreate
// rollouts. It decides what the next revision state is; the controller turns
// that state into Kubernetes objects.
package rollout

import (
	"fmt"
	"time"
)

// Strategy selects how a new revision replaces the stable one.
type Strategy string

const (
	// Canary runs the new revision next to the stable one and shifts traffic
	// through the configured steps.
	Canary Strategy = "Canary"
	// Recreate replaces the stable revision immediately.
	Recreate Strategy = "Recreate"
)

// WarmingStep is the step index of a canary that has been created but has not
// reported Ready yet. It receives no traffic.
const WarmingStep = -1

// State is the persisted rollout state, stored in the ModelDeployment status.
type State struct {
	// Stable is the revision serving production traffic.
	Stable string
	// Canary is the revision being rolled out, empty when none is.
	Canary string
	// Step is the index into Config.Steps, or WarmingStep.
	Step int
	// StepStarted is when the current step (or warm-up) began.
	StepStarted time.Time
	// RolledBack is the last revision that was rolled back. It is not retried
	// until the desired revision changes.
	RolledBack string
}

// Config is the rollout policy from the ModelDeployment spec.
type Config struct {
	Strategy         Strategy
	Steps            []int32
	StepInterval     time.Duration
	ProgressDeadline time.Duration
}

// Observation is what the controller sees in the cluster.
type Observation struct {
	// Desired is the revision computed from the current spec.
	Desired string
	// CanaryReady reports whether the canary revision is Ready.
	CanaryReady bool
	Now         time.Time
}

// Action names the transition taken by Next.
type Action string

const (
	ActionNone        Action = "None"
	ActionInitialize  Action = "Initialize"
	ActionRecreate    Action = "Recreate"
	ActionStartCanary Action = "StartCanary"
	ActionWait        Action = "Wait"
	ActionAdvance     Action = "Advance"
	ActionPromote     Action = "Promote"
	ActionRollback    Action = "Rollback"
	ActionAbort       Action = "Abort"
	ActionHold        Action = "Hold"
)

// Decision is the result of one evaluation of the state machine.
type Decision struct {
	Action Action
	Next   State
	// RequeueAfter is when the state machine wants to be evaluated again even
	// if nothing changes; zero means it only needs to react to events.
	RequeueAfter time.Duration
	Message      string
}

// ValidateSteps checks that steps are strictly increasing percentages ending at 100.
func ValidateSteps(steps []int32) error {
	if len(steps) == 0 {
		return fmt.Errorf("at least one step is required")
	}
	for i, s := range steps {
		if s < 1 || s > 100 {
			return fmt.Errorf("step %d is %d, want 1-100", i, s)
		}
		if i > 0 && s <= steps[i-1] {
			return fmt.Errorf("steps must be strictly increasing, got %d after %d", s, steps[i-1])
		}
	}
	if steps[len(steps)-1] != 100 {
		return fmt.Errorf("the last step must be 100, got %d", steps[len(steps)-1])
	}
	return nil
}

// Weight is the percentage of traffic to route to the canary of s. A canary
// that is warming up or not Ready gets no traffic.
func Weight(s State, steps []int32, canaryReady bool) int32 {
	if s.Canary == "" || s.Step == WarmingStep || !canaryReady {
		return 0
	}
	return StepWeight(steps, s.Step)
}

// StepWeight is the traffic weight a canary at step is provisioned for; a
// warming canary is provisioned for the first step.
func StepWeight(steps []int32, step int) int32 {
	if len(steps) == 0 {
		return 0
	}
	return steps[clampStep(steps, step)]
}

func clampStep(steps []int32, step int) int {
	return max(0, min(step, len(steps)-1))
}

// Next evaluates the state machine. It never mutates cur.
//
// A canary first warms up with no traffic. Once it reports Ready it moves to
// step 0, and after each StepInterval during which it is Ready it moves to the
// next step. After the interval of the last step (100%) it is promoted to
// stable. If the canary is not Ready within ProgressDeadline of the start of
// the current step it is rolled back. While the canary is not Ready its
// traffic weight is 0.
func Next(cur State, cfg Config, obs Observation) Decision {
	desired := obs.Desired
	switch {
	case cur.Stable == "":
		return Decision{
			Action:  ActionInitialize,
			Next:    State{Stable: desired},
			Message: fmt.Sprintf("deploying initial revision %s", desired),
		}

	case desired == cur.Stable:
		next := State{Stable: cur.Stable}
		if cur.Canary != "" {
			return Decision{Action: ActionAbort, Next: next,
				Message: fmt.Sprintf("spec reverted to stable revision %s; removed canary %s", cur.Stable, cur.Canary)}
		}
		return Decision{Action: ActionNone, Next: next}

	case cfg.Strategy == Recreate:
		return Decision{
			Action:  ActionRecreate,
			Next:    State{Stable: desired},
			Message: fmt.Sprintf("replacing revision %s with %s", cur.Stable, desired),
		}

	case desired == cur.RolledBack && cur.Canary == "":
		return Decision{
			Action:  ActionHold,
			Next:    cur,
			Message: fmt.Sprintf("revision %s was rolled back; change the spec to roll out again", desired),
		}

	case cur.Canary != desired:
		return Decision{
			Action:       ActionStartCanary,
			Next:         State{Stable: cur.Stable, Canary: desired, Step: WarmingStep, StepStarted: obs.Now},
			RequeueAfter: cfg.ProgressDeadline,
			Message:      fmt.Sprintf("started canary %s next to stable %s", desired, cur.Stable),
		}
	}

	// A canary for the desired revision is in progress.
	step := cur.Step
	if step != WarmingStep {
		step = clampStep(cfg.Steps, step)
	}
	elapsed := obs.Now.Sub(cur.StepStarted)
	inProgress := State{Stable: cur.Stable, Canary: cur.Canary, Step: step, StepStarted: cur.StepStarted}

	if !obs.CanaryReady {
		if elapsed >= cfg.ProgressDeadline {
			return Decision{
				Action:  ActionRollback,
				Next:    State{Stable: cur.Stable, RolledBack: cur.Canary},
				Message: fmt.Sprintf("canary %s was not Ready within %s; rolled back to %s", cur.Canary, cfg.ProgressDeadline, cur.Stable),
			}
		}
		return Decision{
			Action:       ActionWait,
			Next:         inProgress,
			RequeueAfter: cfg.ProgressDeadline - elapsed,
			Message:      fmt.Sprintf("waiting for canary %s to become Ready", cur.Canary),
		}
	}

	if step == WarmingStep {
		next := inProgress
		next.Step, next.StepStarted = 0, obs.Now
		return Decision{
			Action:       ActionAdvance,
			Next:         next,
			RequeueAfter: cfg.StepInterval,
			Message:      fmt.Sprintf("canary %s is Ready; sending it %d%% of traffic", cur.Canary, cfg.Steps[0]),
		}
	}

	if elapsed < cfg.StepInterval {
		return Decision{
			Action:       ActionWait,
			Next:         inProgress,
			RequeueAfter: cfg.StepInterval - elapsed,
			Message:      fmt.Sprintf("canary %s at %d%%", cur.Canary, cfg.Steps[step]),
		}
	}

	if step == len(cfg.Steps)-1 {
		return Decision{
			Action:  ActionPromote,
			Next:    State{Stable: cur.Canary},
			Message: fmt.Sprintf("promoted canary %s to stable; removed %s", cur.Canary, cur.Stable),
		}
	}

	next := inProgress
	next.Step, next.StepStarted = step+1, obs.Now
	return Decision{
		Action:       ActionAdvance,
		Next:         next,
		RequeueAfter: cfg.StepInterval,
		Message:      fmt.Sprintf("canary %s advanced to %d%%", cur.Canary, cfg.Steps[step+1]),
	}
}
