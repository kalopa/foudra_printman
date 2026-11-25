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
package bambu

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"io"

	"github.com/kalopa/print_manager/internal/printer"
)

// Driver is a Client seen through the daemon's printer vocabulary.
type Driver struct{ C *Client }

var _ printer.Driver = Driver{} // the compiler checks Driver satisfies the interface

// Status maps the printer's gcode_state. Like PrusaLink, a Bambu printer
// holds FINISH (and FAILED) until the next job starts, so the farm's
// "parts on the bed" question needs no tricks here.
func (d Driver) Status(ctx context.Context) (*printer.Status, error) {
	r, err := d.C.Report(ctx)
	if err != nil {
		return nil, err
	}
	out := &printer.Status{State: stateOf(r), Raw: string(r.GCodeState)}
	if r.GCodeState == StateFailed && r.PrintError != 0 && r.PrintError != ErrCancelled {
		out.Raw = fmt.Sprintf("FAILED (error %08X)", r.PrintError)
	}
	name := cmp.Or(r.SubtaskName, r.GCodeFile)
	if r.GCodeState == StateIdle || name == "" {
		return out, nil
	}
	out.Job = &printer.Job{
		ID:            jobID(r),
		File:          name,
		Progress:      float64(r.Percent),
		TimeRemaining: r.RemainingMin * 60,
	}
	return out, nil
}

func stateOf(r *Report) printer.State {
	switch r.GCodeState {
	case StateIdle:
		return printer.Idle
	case StatePrepare, StateSlicing:
		return printer.Busy
	case StateRunning:
		return printer.Printing
	case StatePause:
		return printer.Paused
	case StateFinish:
		return printer.Finished
	case StateFailed:
		if r.PrintError == 0 || r.PrintError == ErrCancelled {
			return printer.Stopped
		}
		return printer.Error
	default:
		return printer.Attention
	}
}

// jobID makes a stable number for one print from what the printer says
// about it. In LAN mode the printer's task ids are often "0", so the
// start time does the work of telling a reprint of the same file from
// the last one; the ids are mixed in for firmware that does echo them.
// Positive and never zero, which the farm reads as "no job".
func jobID(r *Report) int {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s", cmp.Or(r.SubtaskName, r.GCodeFile), r.GCodeStartTime, r.TaskID, r.SubtaskID, r.JobID)
	id := int(h.Sum32() & 0x7fffffff)
	if id == 0 {
		id = 1
	}
	return id
}

func (d Driver) Upload(ctx context.Context, name string, body io.Reader, size int64, printNow bool) error {
	return d.C.Upload(ctx, name, body, size, printNow)
}
