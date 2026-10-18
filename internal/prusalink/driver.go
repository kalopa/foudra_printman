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
package prusalink

import (
	"context"
	"io"

	"github.com/kalopa/print_manager/internal/printer"
)

// Driver is a Client seen through the daemon's printer vocabulary.
type Driver struct{ C *Client }

var _ printer.Driver = Driver{} // the compiler checks Driver satisfies the interface

// states maps PrusaLink's words onto the daemon's. READY is the
// printer's "idle, and marked ready for the next job" — idle either way.
var states = map[State]printer.State{
	StateIdle:      printer.Idle,
	StateReady:     printer.Idle,
	StateBusy:      printer.Busy,
	StatePrinting:  printer.Printing,
	StatePaused:    printer.Paused,
	StateFinished:  printer.Finished,
	StateStopped:   printer.Stopped,
	StateError:     printer.Error,
	StateAttention: printer.Attention,
}

// Status reads the status and, when there is a job, the job too: the
// status carries progress, but only /api/v1/job names the file. The
// second read is best effort, as a missing file name only makes a
// note less helpful.
func (d Driver) Status(ctx context.Context) (*printer.Status, error) {
	s, err := d.C.Status(ctx)
	if err != nil {
		return nil, err
	}
	st, ok := states[s.Printer.State]
	if !ok {
		st = printer.Attention
	}
	out := &printer.Status{State: st, Raw: string(s.Printer.State)}
	if s.Job != nil {
		out.Job = &printer.Job{ID: s.Job.ID, Progress: s.Job.Progress, TimeRemaining: s.Job.TimeRemaining}
		if j, err := d.C.Job(ctx); err == nil && j != nil {
			out.Job.File = j.File.DisplayName
		}
	}
	return out, nil
}

func (d Driver) Upload(ctx context.Context, name string, body io.Reader, size int64, printNow bool) error {
	return d.C.Upload(ctx, name, body, size, printNow)
}
