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
package octoprint

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"

	"github.com/kalopa/print_manager/internal/printer"
)

// Driver is a Client seen through the daemon's printer vocabulary.
type Driver struct{ C *Client }

var _ printer.Driver = Driver{} // the compiler checks Driver satisfies the interface

// Status is up to three reads: the printer's flags, the selected job,
// and — when the printer is idle with a file selected — that file's
// print history.
//
// The history is the delicate part. PrusaLink says FINISHED until an
// operator taps the screen; OctoPrint goes straight back to Operational.
// Reported as idle, that would have the farm start the next job on top
// of the parts. Upload deletes a file before replacing it, which clears
// its print history, so a selected file with any history at all has
// been printed since it arrived: that print has ended, finished if it
// succeeded and stopped if not. The next upload clears it again.
//
// Timestamps cannot do this: OctoPrint records an upload to the whole
// second, and a print that ends in the same second as the next upload
// (seen live) or the same second as its own (a print cancelled at once)
// is ambiguous. A file uploaded over an old one by something other than
// this driver keeps its history and so reads as ended — an operator is
// asked, which is the safe way to be wrong.
func (d Driver) Status(ctx context.Context) (*printer.Status, error) {
	p, err := d.C.Printer(ctx)
	if errors.Is(err, ErrNotOperational) {
		// OctoPrint is up but has no printer: someone must look. The job
		// endpoint still answers, with the operator text
		// ("Offline", "Offline after error: …") worth showing them.
		raw := "Offline"
		if j, err := d.C.Job(ctx); err == nil && j.State != "" {
			raw = j.State
		}
		return &printer.Status{State: printer.Attention, Raw: raw}, nil
	}
	if err != nil {
		return nil, err
	}
	j, err := d.C.Job(ctx)
	if err != nil {
		return nil, err
	}

	out := &printer.Status{State: stateOf(p.State.Flags), Raw: p.State.Text}
	f := j.Job.File
	if f.Path == "" {
		return out, nil // nothing selected, so no job to report
	}
	job := &printer.Job{File: cmp.Or(f.Display, f.Name)}
	if c := j.Progress.Completion; c != nil {
		job.Progress = *c
	}
	if t := j.Progress.PrintTimeLeft; t != nil {
		job.TimeRemaining = *t
	}

	if out.State != printer.Idle {
		job.ID = jobID(f.Path, f.Date, 0)
		out.Job = job
		return out, nil
	}
	// Idle with a file selected: has a print of it ended since it came?
	// If the history can't be read, say so rather than guess idle; a
	// wrong idle prints on top of the last job's parts.
	h, err := d.C.File(ctx, f.Origin, f.Path)
	if err != nil {
		return nil, fmt.Errorf("print history of %s: %w", f.Path, err)
	}
	if h.Prints == nil || h.Prints.Last == nil {
		return out, nil // not printed since it was uploaded: ready for a job
	}
	out.State = printer.Stopped
	if h.Prints.Last.Success {
		out.State = printer.Finished
	}
	job.ID = jobID(f.Path, f.Date, h.Prints.Last.Date)
	out.Job = job
	return out, nil
}

// stateOf reads OctoPrint's flags in order of precedence: several are
// set at once (a cancelling printer is also printing).
func stateOf(f Flags) printer.State {
	switch {
	case f.Error:
		return printer.Error
	case f.Cancelling:
		return printer.Busy // the job hasn't ended yet; it will end as stopped
	case f.Pausing, f.Paused:
		return printer.Paused
	case f.Printing, f.Finishing, f.Resuming:
		return printer.Printing
	case f.Operational:
		return printer.Idle
	default:
		return printer.Attention
	}
}

// jobID makes the job number OctoPrint doesn't have: the same print
// always gets the same number, across looks and across restarts, and
// a reprint of the same file gets a new one because its end time
// differs. ended is zero while the print is still running. Kept
// positive, and never zero, which the farm reads as "no job".
func jobID(path string, uploaded int64, ended float64) int {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s|%d|%.3f", path, uploaded, ended)
	id := int(h.Sum32() & 0x7fffffff)
	if id == 0 {
		id = 1
	}
	return id
}

func (d Driver) Upload(ctx context.Context, name string, body io.Reader, size int64, printNow bool) error {
	return d.C.Upload(ctx, name, body, size, printNow)
}
