//
// Copyright (c) 2026, Kalopa Robotics Limited.
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
//
// Package printer is the daemon's own vocabulary for a printer,
// whatever protocol it speaks. Each protocol (PrusaLink today,
// OctoPrint next) is a Driver that translates its own states and shapes
// into these, so the loop in internal/farm never learns a protocol's
// words.
package printer

import (
	"context"
	"io"
)

// State is what the loop needs to know, and no more. A driver maps its
// protocol's states onto these; anything it cannot place is Attention,
// because an unknown state is a reason for an operator to look, never to
// start a print.
type State string

const (
	Idle      State = "idle"      // ready for a job
	Busy      State = "busy"      // occupied, but not printing (homing, calibrating, ...)
	Printing  State = "printing"  // a job is running
	Paused    State = "paused"    // a job is running but held
	Finished  State = "finished"  // a job ended normally; parts are on the bed
	Stopped   State = "stopped"   // a job was cancelled; maybe a mess on the bed
	Error     State = "error"     // a job ended badly, or the printer is faulted
	Attention State = "attention" // the printer wants an operator at its screen
)

// Status is one look at a printer.
type Status struct {
	State State
	// Raw is the protocol's own word for the state ("IDLE",
	// "Operational"), for notes and logs that operators read.
	Raw string
	// Job is the current or last job, nil when the printer reports none.
	Job *Job
}

// Job is the job a printer reports.
type Job struct {
	// ID identifies the job across looks at the printer, so a finished
	// job's bed is asked about once. Zero when the protocol has none.
	ID            int
	File          string  // the file's name as an operator knows it
	Progress      float64 // percent
	TimeRemaining int     // seconds, the printer's own live estimate
}

// Driver is one printer, spoken to in its own protocol.
type Driver interface {
	Status(ctx context.Context) (*Status, error)
	// Upload sends a file of size bytes under name; with printNow the
	// printer starts it as soon as it has arrived.
	Upload(ctx context.Context, name string, body io.Reader, size int64, printNow bool) error
}
