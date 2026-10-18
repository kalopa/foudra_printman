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
// Package farm is the daemon: discover the printers from the MRP, and
// for each one keep the MRP honest about what the printer is doing.
//
// The loop: mirror PrusaLink's state onto the workstation; when the
// printer is idle and inside its window, ask the MRP for the next job
// in the loaded material, send its G-code and claim the step; when a
// print finishes, block the station and raise "clear the bed"; once an
// operator has tapped Ready, complete the step for one bed with the
// slicer's filament figure as the actual. The last step's completion is
// what receives the part into stock — the whole path from a released
// work order to a stock increase, with an operator only at the bed.
//
// It is stateless across restarts on purpose. The little it must
// remember between ticks — which finished job is awaiting clearance,
// which was last completed — lives in the workstation's custom JSON in
// the MRP, so a restarted daemon picks up exactly where it was and two
// daemons cannot disagree.
package farm

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kalopa/print_manager/internal/mrp"
	"github.com/kalopa/print_manager/internal/octoprint"
	"github.com/kalopa/print_manager/internal/printer"
	"github.com/kalopa/print_manager/internal/prusalink"
)

// TaskSource is what this daemon's tasks are filed under in the MRP.
const TaskSource = "print-farm"

// autoTag marks a workstation status the daemon wrote. It never
// overwrites a status an operator set (no tag) unless the printer is
// demonstrably printing — busy is a machine fact.
const autoTag = "[auto] "

// Settings is the non-secret half of a printer's configuration, read
// from its workstation's custom JSON in the MRP. The secret half — the
// PrusaLink key — is an environment variable named by the workstation
// code: PRUSALINK_KEY_<CODE>.
type Settings struct {
	// Driver is the protocol the printer speaks: prusalink (the default,
	// so stations configured before there was a choice need no change)
	// or octoprint.
	Driver string `json:"driver,omitempty"`
	// URL is the printer's address for any driver. PrusaLinkURL is the
	// older name, still read for prusalink stations; see Address.
	URL          string `json:"url,omitempty"`
	PrusaLinkURL string `json:"prusalink_url,omitempty"`
	Profile      string `json:"profile"`
	Window       struct {
		Start              string `json:"start"`
		End                string `json:"end"`
		Timezone           string `json:"timezone"`
		MustFinishInWindow bool   `json:"must_finish_in_window"`
	} `json:"window"`
	FilamentPolicy struct {
		ChangeForPriorityAtOrAbove int `json:"change_for_priority_at_or_above"`
	} `json:"filament_policy"`

	// The daemon's own bookkeeping, kept here so it survives restarts.
	AwaitingClearJob int `json:"awaiting_clear_job"` // PrusaLink job id whose bed needs clearing
	LastCompletedJob int `json:"last_completed_job"`
	// The run in progress, from the moment the file is sent until the
	// bed is cleared: which MRP step, how many per bed, and what the
	// slicer said one bed uses — so completion can report actuals.
	Run *Run `json:"run,omitempty"`
}

// Address is where the printer is: url, or the older prusalink_url.
func (s Settings) Address() string { return cmp.Or(s.URL, s.PrusaLinkURL) }

// Run is one print of one bed, as started by the daemon.
type Run struct {
	StepID         int64   `json:"step_id"`
	WorkOrder      string  `json:"work_order"`
	File           string  `json:"file"`
	PerBed         string  `json:"per_bed"`
	FilamentPartID int64   `json:"filament_part_id,omitempty"`
	FilamentG      float64 `json:"filament_g,omitempty"`
	StartedAt      string  `json:"started_at"`
	// JobID is PrusaLink's id for the print, learned on the first tick
	// that sees it running. It lets a FINISHED printer that has already
	// forgotten its job (dismissed on the screen, or the daemon was
	// away) still be matched to the run it belongs to.
	JobID int `json:"job_id,omitempty"`
}

// Sidecar is what slice.sh writes beside each .bgcode.
type Sidecar struct {
	PerBed     json.Number `json:"per_bed"`
	EstSeconds int         `json:"est_seconds"`
	FilamentG  float64     `json:"filament_g"`
	Material   string      `json:"material"`
}

// Farm-wide configuration that is not per printer.
type Config struct {
	GCodeDir string // where a part's G-code name resolves when it is not a URL
}

// Printer is one discovered station and its client.
type Printer struct {
	WS       mrp.Workstation
	Settings Settings
	Link     printer.Driver
}

// The MRP seam the loop talks through, so tests can fake it. The
// printer's seam is printer.Driver.
type mrpAPI interface {
	Workstations(ctx context.Context) ([]mrp.Workstation, error)
	Workstation(ctx context.Context, id int64) (*mrp.Workstation, error)
	SetStatus(ctx context.Context, id int64, status string, note *string) error
	Custom(ctx context.Context, kind string, id int64) (json.RawMessage, error)
	SetCustom(ctx context.Context, kind string, id int64, v any) error
	WorkOrders(ctx context.Context, status string) ([]mrp.WorkOrder, error)
	WorkOrder(ctx context.Context, id int64) (*mrp.WorkOrder, error)
	CompleteStep(ctx context.Context, stepID int64, qty string, consumed []mrp.Consumed) error
	PauseStep(ctx context.Context, stepID int64) error
	StartStep(ctx context.Context, stepID, wsID int64) error
	ScrapStep(ctx context.Context, stepID int64, qty string, consumed []mrp.Consumed, note string) error
	SetMaterial(ctx context.Context, id int64, partID *int64) error
	ReplaceTasks(ctx context.Context, source string, tasks []mrp.Task) error
	TaskStates(ctx context.Context, source string) ([]mrp.TaskState, error)
	Next(ctx context.Context, wsID int64, material *int64) (*mrp.Job, error)
}

// KeyEnvFor is the environment variable holding a station's printer
// key: PRINTER_KEY_ plus the code upper-cased with non-alphanumerics as
// underscores. CORE_ONEP_1 → PRINTER_KEY_CORE_ONEP_1.
func KeyEnvFor(code string) string { return envName("PRINTER_KEY_", code) }

// legacyKeyEnvFor is the name keys had when every printer spoke
// PrusaLink, still read so an existing Pi keeps working:
// PRUSALINK_KEY_CORE_ONEP_1.
func legacyKeyEnvFor(code string) string { return envName("PRUSALINK_KEY_", code) }

// printerKey finds a station's key under either name.
func printerKey(code string) string {
	return cmp.Or(os.Getenv(KeyEnvFor(code)), os.Getenv(legacyKeyEnvFor(code)))
}

func envName(prefix, code string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, r := range strings.ToUpper(code) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Discover asks the MRP for the fleet: automatic, active stations whose
// custom JSON names a printer address, and for which a key is set.
// Stations missing either are logged and skipped, never guessed at.
func Discover(ctx context.Context, m mrpAPI, log *slog.Logger) ([]*Printer, error) {
	all, err := m.Workstations(ctx)
	if err != nil {
		return nil, err
	}
	var fleet []*Printer
	for _, ws := range all {
		if ws.Kind != "automatic" || !ws.IsActive {
			continue
		}
		raw, err := m.Custom(ctx, "workstations", ws.ID)
		if err != nil {
			return nil, fmt.Errorf("custom for %s: %w", ws.Code, err)
		}
		var st Settings
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &st); err != nil {
				log.Warn("workstation custom JSON unreadable; skipping", "workstation", ws.Code, "err", err)
				continue
			}
		}
		if st.Address() == "" {
			log.Info("automatic workstation has no printer url; not a printer we drive", "workstation", ws.Code)
			continue
		}
		key := printerKey(ws.Code)
		if key == "" {
			log.Warn("no printer key for workstation; skipping", "workstation", ws.Code, "env", KeyEnvFor(ws.Code))
			continue
		}
		var link printer.Driver
		switch st.Driver {
		case "", "prusalink":
			c, err := prusalink.New(st.Address(), key)
			if err != nil {
				log.Warn("bad printer url; skipping", "workstation", ws.Code, "err", err)
				continue
			}
			link = prusalink.Driver{C: c}
		case "octoprint":
			c, err := octoprint.New(st.Address(), key)
			if err != nil {
				log.Warn("bad printer url; skipping", "workstation", ws.Code, "err", err)
				continue
			}
			link = octoprint.Driver{C: c}
		default:
			log.Warn("unknown printer driver; skipping", "workstation", ws.Code, "driver", st.Driver)
			continue
		}
		fleet = append(fleet, &Printer{WS: ws, Settings: st, Link: link})
	}
	return fleet, nil
}

// Farm runs the loop.
type Farm struct {
	MRP  mrpAPI
	Log  *slog.Logger
	Poll time.Duration
	Cfg  Config
	// now is a seam for the window tests; nil means time.Now.
	now func() time.Time

	// Each printer runs in a goroutine of its own (runFleet), and these
	// are the only things they share. Everything else a goroutine
	// touches belongs to its own *Printer.
	//
	// mu guards outcomes and lastWhy, and is only ever held briefly.
	mu sync.Mutex
	// outcomes is what operators chose on this daemon's tasks,
	// refreshed before each tick: key -> outcome.
	outcomes map[string]string
	// lastWhy is the last logged reason each printer is not printing,
	// by workstation code; see whyIdle.
	lastWhy map[string]string

	// pubMu guards tasks and is held across the MRP call that publishes
	// them; see publishTasks.
	pubMu sync.Mutex
	// tasks is each printer's open tasks as last published, by
	// workstation code. Their union is what the MRP holds.
	tasks map[string][]mrp.Task
}

// refreshOutcomes reads back the decisions made on our tasks.
func (f *Farm) refreshOutcomes(ctx context.Context) {
	states, err := f.MRP.TaskStates(ctx, TaskSource)
	if err != nil {
		f.Log.Warn("read task outcomes", "err", err)
		return
	}
	// Build the new map first and swap it in under the lock, so the
	// lock is never held across the network call above.
	outcomes := map[string]string{}
	for _, t := range states {
		if t.Outcome != nil {
			outcomes[t.Key] = *t.Outcome
		}
	}
	f.mu.Lock()
	f.outcomes = outcomes
	f.mu.Unlock()
}

func (f *Farm) outcome(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outcomes[key]
}

func (f *Farm) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// Run discovers the fleet and drives every printer until ctx ends.
func (f *Farm) Run(ctx context.Context) error {
	fleet, err := Discover(ctx, f.MRP, f.Log)
	if err != nil {
		return fmt.Errorf("discover: %w", err)
	}
	if len(fleet) == 0 {
		return errors.New("no printers to drive: no automatic workstation has a printer url and a key")
	}
	for _, p := range fleet {
		f.Log.Info("driving printer", "workstation", p.WS.Code, "id", p.WS.ID, "driver", cmp.Or(p.Settings.Driver, "prusalink"), "url", p.Settings.Address())
	}
	f.runFleet(ctx, fleet)
	return nil
}

// runFleet drives each printer in a goroutine of its own, and returns
// once ctx has ended and every one of them has stopped.
//
// A goroutine per printer so that a slow printer delays only itself: a
// fifteen-minute upload to one, or one timing out on every call, used
// to hold up every other printer's tick behind it. The WaitGroup is how
// runFleet knows they have all finished: Add before each goroutine
// starts, Done as each returns, Wait blocks until the count is back to
// zero — so the process never exits halfway through a printer's tick.
func (f *Farm) runFleet(ctx context.Context, fleet []*Printer) {
	var wg sync.WaitGroup
	for _, p := range fleet {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.drive(ctx, p)
		}()
	}
	wg.Wait()
}

// drive is one printer's loop: look, act, publish its tasks, wait.
func (f *Farm) drive(ctx context.Context, p *Printer) {
	t := time.NewTicker(f.Poll)
	defer t.Stop()
	for {
		f.refreshOutcomes(ctx)
		tasks, err := f.Tick(ctx, p)
		switch {
		case ctx.Err() != nil:
			return // shutting down; whatever failed, failed because of that
		case err != nil:
			// Leave this printer's last tasks open: a failed look at the
			// MRP is no reason to resolve "clear the bed".
			f.Log.Error("tick failed", "workstation", p.WS.Code, "err", err)
		default:
			f.publishTasks(ctx, p.WS.Code, tasks)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// publishTasks records one printer's open tasks and sends the MRP the
// union of every printer's — the daemon's complete set, wholesale.
//
// pubMu is held across the PUT on purpose. Two printers publishing at
// once would otherwise race: each builds a union, and the slower PUT,
// carrying the older one, could land last and undo the other's change.
// It is a separate lock from mu so that a slow MRP stalls only other
// publishes, never a printer's tick.
func (f *Farm) publishTasks(ctx context.Context, code string, tasks []mrp.Task) {
	f.pubMu.Lock()
	defer f.pubMu.Unlock()
	if f.tasks == nil {
		f.tasks = map[string][]mrp.Task{}
	}
	f.tasks[code] = tasks
	// Map order is random in Go; sort so the MRP sees the same list for
	// the same tasks.
	codes := make([]string, 0, len(f.tasks))
	for c := range f.tasks {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	var all []mrp.Task
	for _, c := range codes {
		all = append(all, f.tasks[c]...)
	}
	if err := f.MRP.ReplaceTasks(ctx, TaskSource, all); err != nil {
		f.Log.Error("replace tasks", "err", err)
	}
}

// mirror maps a printer's state onto the MRP's four.
func mirror(s printer.State) string {
	switch s {
	case printer.Idle:
		return "idle"
	case printer.Printing, printer.Busy, printer.Paused:
		return "busy"
	default: // finished, stopped, error, attention
		return "blocked"
	}
}

// Tick reconciles one printer once and returns the tasks it wants open.
func (f *Farm) Tick(ctx context.Context, p *Printer) ([]mrp.Task, error) {
	ws, err := f.MRP.Workstation(ctx, p.WS.ID)
	if err != nil {
		return nil, fmt.Errorf("read workstation: %w", err)
	}
	ownsStatus := ws.StatusNote != nil && strings.HasPrefix(*ws.StatusNote, autoTag) || ws.Status == "busy"

	st, err := p.Link.Status(ctx)
	if err != nil {
		// Logged whatever the station's status, so a printer that drops
		// off the network while the station is already offline, or held
		// by an operator, is not silent.
		f.whyIdle(ws.Code, "printer unreachable: "+err.Error())
		// Unreachable: say so, but only over a status we set ourselves.
		if ownsStatus && ws.Status != "offline" {
			note := autoTag + "unreachable: " + err.Error()
			if serr := f.MRP.SetStatus(ctx, ws.ID, "offline", &note); serr != nil {
				return nil, serr
			}
			f.Log.Warn("printer unreachable; marked offline", "workstation", ws.Code, "err", err)
		}
		return nil, nil
	}

	var tasks []mrp.Task

	switch st.State {
	case printer.Finished, printer.Stopped, printer.Error:
		// Parts (or a mess) are on the bed. First sight: block and ask.
		// Later sights while the operator hasn't tapped a button: nothing
		// new to say. A stopped or errored print asks the same question
		// as a finished one — was anything usable? — with the answer
		// usually being Print failed, which scraps the run's filament
		// and leaves the step to try again.
		jobID := 0
		switch {
		case st.Job != nil:
			jobID = st.Job.ID
		case p.Settings.Run != nil:
			// The printer no longer reports the job but we started one,
			// so this is its end. Use the id we learned while it ran, or
			// a stand-in unique to the run when we never saw it running.
			jobID = p.Settings.Run.JobID
			if jobID == 0 {
				jobID = -int(p.Settings.Run.StepID)
			}
		}
		if p.Settings.AwaitingClearJob != jobID && p.Settings.LastCompletedJob != jobID {
			p.Settings.AwaitingClearJob = jobID
			if err := f.MRP.SetCustom(ctx, "workstations", ws.ID, p.Settings); err != nil {
				return nil, err
			}
			note := autoTag + "bed needs clearing"
			if st.State != printer.Finished {
				note = autoTag + "print " + strings.ToLower(st.Raw) + "; bed needs clearing"
			}
			if err := f.MRP.SetStatus(ctx, ws.ID, "blocked", &note); err != nil {
				return nil, err
			}
			f.Log.Info("print ended; awaiting bed clear", "workstation", ws.Code, "job", jobID, "printer", st.Raw)
		}
		settledNow := false // a run was closed in this tick
		if p.Settings.AwaitingClearJob == jobID && jobID != 0 {
			msg := fmt.Sprintf("%s: print finished — clear the bed and check the parts.", ws.Name)
			if st.State != printer.Finished {
				msg = fmt.Sprintf("%s: print %s — clear the bed. Anything usable? If not, Print failed scraps the filament and the step is printed again.",
					ws.Name, strings.ToLower(st.Raw))
			}
			ts, settled, err := f.settleBed(ctx, p, ws, jobID, msg, ws.Status == "idle")
			if err != nil {
				return nil, err
			}
			tasks, settledNow = append(tasks, ts...), settled
			if !settled {
				f.whyIdle(ws.Code, "bed needs clearing")
			}
		}
		// Bed cleared and nothing pending: the FINISHED on the screen is
		// only the printer waiting for someone to tap it. Treat it as
		// idle and offer the next job; if the printer refuses the upload
		// while in this state, that comes back as the usual blocked
		// status and task rather than a silent wait.
		// One thing per tick: a run closed just now starts nothing until
		// the next look, so the books settle before the printer is asked.
		if !settledNow && p.Settings.AwaitingClearJob == 0 && p.Settings.Run == nil && ws.Status == "idle" {
			ts, err := f.startNext(ctx, p, ws)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, ts...)
		}

	default:
		want := mirror(st.State)
		if want == "idle" && p.Settings.AwaitingClearJob != 0 {
			// The printer is idle but its last bed was never answered:
			// someone dismissed FINISHED on the screen first. The bed
			// question still stands, so keep the station blocked and keep
			// asking. Only the task's buttons answer it here — the
			// station's idle may be our own mirror of the printer, which
			// says nothing about the parts.
			if ownsStatus && ws.Status != "blocked" {
				note := autoTag + "bed needs clearing (print dismissed on the printer)"
				if err := f.MRP.SetStatus(ctx, ws.ID, "blocked", &note); err != nil {
					return nil, err
				}
			}
			jobID := p.Settings.AwaitingClearJob
			what := "the last print"
			if r := p.Settings.Run; r != nil {
				what = fmt.Sprintf("%s for %s", r.File, r.WorkOrder)
			}
			msg := fmt.Sprintf("%s: %s ended and was dismissed on the printer without an answer — is the bed clear and were the parts good?", ws.Name, what)
			ts, settled, err := f.settleBed(ctx, p, ws, jobID, msg, false)
			if err != nil {
				return nil, err
			}
			if !settled {
				f.whyIdle(ws.Code, "bed needs clearing (print dismissed on the printer)")
			}
			return ts, nil
		}
		note := autoTag + st.Raw
		if st.Job != nil {
			if st.Job.File != "" {
				note = fmt.Sprintf("%s%s %.0f%%, %s", autoTag, st.Job.File, st.Job.Progress, hms(st.Job.TimeRemaining))
			}
			// Remember which PrusaLink job our run became.
			if r := p.Settings.Run; r != nil && r.JobID == 0 && st.Job.ID != 0 {
				r.JobID = st.Job.ID
				_ = f.MRP.SetCustom(ctx, "workstations", ws.ID, p.Settings) // best effort; relearned next tick
			}
		}
		// busy is a machine fact and always written; idle/blocked only
		// replace a status we wrote (an operator's "offline" stands).
		if ws.Status != want && (want == "busy" || ownsStatus) {
			if err := f.MRP.SetStatus(ctx, ws.ID, want, &note); err != nil {
				return nil, err
			}
			f.Log.Info("mirrored printer state", "workstation", ws.Code, "printer", st.Raw, "mrp", want)
		} else if ws.Status == "busy" && want == "busy" && (ws.StatusNote == nil || *ws.StatusNote != note) {
			_ = f.MRP.SetStatus(ctx, ws.ID, want, &note) // progress in the note; best effort
		}
		switch {
		case want == "blocked":
			// ATTENTION, or a state this client doesn't know: the printer
			// wants an operator at its screen.
			f.whyIdle(ws.Code, "printer reports "+st.Raw)
		case want != "idle":
			// Printing, or paused: not idle, nothing to explain.
			f.whyIdle(ws.Code, "")
		case ws.Status == "idle":
			ts, err := f.startNext(ctx, p, ws)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, ts...)
		case !ownsStatus:
			f.whyIdle(ws.Code, "station is "+ws.Status+", set by an operator")
		}
	}
	return tasks, nil
}

// settleBed asks about the bed left by jobID, or acts on the answer.
// readyByStatus lets the station's idle (Ready on the workstation page)
// count as "parts good"; the caller says whether that idle can be
// trusted to be an operator's. settled reports that the run was closed.
func (f *Farm) settleBed(ctx context.Context, p *Printer, ws *mrp.Workstation, jobID int, msg string, readyByStatus bool) ([]mrp.Task, bool, error) {
	bedKey := fmt.Sprintf("%s-bed-%d", ws.Code, jobID)
	switch {
	case f.outcome(bedKey) == "failed":
		// The parts came off the bed wrong: the filament is gone,
		// nothing was made, the step stays open to try again.
		if err := f.scrapFinishedRun(ctx, p, ws, jobID); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	case f.outcome(bedKey) == "ready" || readyByStatus:
		// Bed clear and parts checked. Record the run.
		if err := f.completeFinishedRun(ctx, p, ws, jobID); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	actions := []mrp.Action{{Key: "ready", Label: "Bed clear, parts good", Tone: "primary"}}
	if p.Settings.Run != nil {
		actions = append(actions, mrp.Action{Key: "failed", Label: "Print failed", Tone: "danger"})
	}
	return []mrp.Task{{
		Key: bedKey, Priority: 1,
		Message: msg,
		Href:    fmt.Sprintf("/workstations/%d", ws.ID), Actions: actions,
	}}, false, nil
}

// whyIdle logs why an idle printer is not printing, once per change of
// reason, so the journal answers "why isn't it printing?" without a
// line every tick. An empty reason means it is printing (or about to);
// the next reason will be logged afresh. Memory only — losing it on
// restart costs one repeated log line.
func (f *Farm) whyIdle(code, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastWhy == nil {
		f.lastWhy = map[string]string{}
	}
	if f.lastWhy[code] == reason {
		return
	}
	f.lastWhy[code] = reason
	if reason != "" {
		f.Log.Info("not starting a print", "workstation", code, "why", reason)
	}
}

// startNext is the daemon's one PrusaLink write: pick the job, send the
// file, tell the MRP. The policy is:
//
//  1. anything ready in what is loaded? print it;
//  2. otherwise, what would I have to change to? ask, and wait;
//
// with the operating window and the part's G-code checked in between.
// Every reason not to print is either silent (window) or a task naming
// what an operator must do (no G-code, wrong material).
// jobRef is a job to print: from next-job, or a step this station
// already holds.
type jobRef struct {
	*mrp.Job
	resumed bool
}

func (f *Farm) startNext(ctx context.Context, p *Printer, ws *mrp.Workstation) ([]mrp.Task, error) {
	href := fmt.Sprintf("/workstations/%d", ws.ID)
	var loaded *int64
	if ws.CurrentMaterial != nil {
		loaded = &ws.CurrentMaterial.PartID
	}

	// A step this station already holds — a second bed, or a retry after
	// a failed print — is not "next", it is ours: next-job only offers
	// pending steps, so resume before asking.
	job, err := f.resumeRunning(ctx, ws)
	if err != nil {
		return nil, err
	}
	if job == nil {
		next, err := f.MRP.Next(ctx, ws.ID, loaded)
		if err != nil {
			return nil, fmt.Errorf("next: %w", err)
		}
		if next != nil {
			job = &jobRef{Job: next}
		}
	}
	if job == nil && loaded != nil {
		other, err := f.MRP.Next(ctx, ws.ID, nil)
		if err != nil {
			return nil, fmt.Errorf("next: %w", err)
		}
		if other != nil && other.RequiresMaterialChange {
			want := "another material"
			var wantID int64
			for _, m := range other.Materials {
				if m.UOM == "kg" || m.UOM == "g" {
					want, wantID = m.PartNumber+" "+m.Name, m.PartID
					break
				}
			}
			key := fmt.Sprintf("%s-material-%d", ws.Code, wantID)
			f.whyIdle(ws.Code, fmt.Sprintf("waiting for a filament change to %s for %s", want, other.WorkOrder.Number))
			if f.outcome(key) == "loaded" && wantID != 0 {
				// The operator fitted the spool and
				// pressed the button: record the
				// material, and the next tick prints
				// in it.
				if err := f.MRP.SetMaterial(ctx, ws.ID, &wantID); err != nil {
					return nil, err
				}
				f.Log.Info("material changed by button", "workstation", ws.Code, "material", want)
				return nil, nil
			}
			return []mrp.Task{{
				Key: key, Priority: 2,
				Message: fmt.Sprintf("%s: change filament from %s to %s for %s (%s).",
					ws.Name, *ws.CurrentMaterial.Name, want, other.WorkOrder.Number, other.Part.Name),
				Href:    href,
				Actions: []mrp.Action{{Key: "loaded", Label: "Loaded " + want, Tone: "primary"}},
			}}, nil
		}
		f.whyIdle(ws.Code, "no ready work in the loaded material")
		return nil, nil
	}
	if job == nil {
		f.whyIdle(ws.Code, "no ready work")
		return nil, nil
	}

	// Where is the file? The part's custom JSON, by this printer's profile.
	name, err := f.gcodeFor(ctx, job.Part.ID, p.Settings.Profile)
	if err != nil {
		return nil, err
	}
	if name == "" {
		f.whyIdle(ws.Code, fmt.Sprintf("%s: part %d has no G-code for profile %s", job.WorkOrder.Number, job.Part.ID, p.Settings.Profile))
		return []mrp.Task{{
			Key: fmt.Sprintf("part-%d-gcode", job.Part.ID), Priority: 2,
			Message: fmt.Sprintf("%s: no G-code for %s (%s) under profile %q — add it to the part's custom data as gcode.%s.",
				ws.Name, job.Part.Name, job.WorkOrder.Number, p.Settings.Profile, p.Settings.Profile),
			Href: fmt.Sprintf("/parts/%d/custom", job.Part.ID),
		}}, nil
	}
	path, side, err := f.resolveGCode(name)
	if err != nil {
		f.whyIdle(ws.Code, fmt.Sprintf("%s: G-code %s not found: %v", job.WorkOrder.Number, name, err))
		return []mrp.Task{{
			Key: fmt.Sprintf("part-%d-gcode", job.Part.ID), Priority: 2,
			Message: fmt.Sprintf("%s: G-code %q for %s is not on the printer host: %v", ws.Name, name, job.Part.Name, err),
			Href:    fmt.Sprintf("/parts/%d/custom", job.Part.ID),
		}}, nil
	}

	// The window: start only if this run fits.
	if ok, why := f.inWindow(p.Settings, side.EstSeconds); !ok {
		f.whyIdle(ws.Code, fmt.Sprintf("%s: outside operating window: %s", job.WorkOrder.Number, why))
		return nil, nil
	}

	perBed := "1"
	if side.PerBed != "" {
		perBed = side.PerBed.String()
	}
	run := &Run{
		StepID: job.StepID, WorkOrder: job.WorkOrder.Number, File: filepath.Base(path),
		PerBed: perBed, FilamentG: side.FilamentG, StartedAt: f.clock().UTC().Format(time.RFC3339),
	}
	if loaded != nil {
		for _, m := range job.Materials {
			if m.PartID == *loaded {
				run.FilamentPartID = m.PartID
			}
		}
	}

	// Order matters: claim the step in the MRP first (it refuses if the
	// step is no longer ready), then send the file. A file sent for a
	// step we could not claim would be a print nobody is accounting for.
	// A step we already hold is already claimed.
	if !job.resumed {
		if err := f.MRP.StartStep(ctx, job.StepID, ws.ID); err != nil {
			return nil, fmt.Errorf("start step %d: %w", job.StepID, err)
		}
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if err := p.Link.Upload(ctx, run.File, fh, info.Size(), true); err != nil {
		f.Log.Warn("upload failed", "workstation", ws.Code, "work_order", run.WorkOrder, "file", run.File, "err", err)
		note := autoTag + "upload failed: " + err.Error()
		_ = f.MRP.SetStatus(ctx, ws.ID, "blocked", &note)
		f.whyIdle(ws.Code, "upload failed")
		return []mrp.Task{{
			Key: fmt.Sprintf("%s-upload", ws.Code), Priority: 1,
			Message: fmt.Sprintf("%s: could not send %s to the printer (%v). The step is started in the MRP; check the printer, then Skip or Start it again.", ws.Name, run.File, err),
			Href:    href,
		}}, nil
	}
	p.Settings.Run = run
	if err := f.MRP.SetCustom(ctx, "workstations", ws.ID, p.Settings); err != nil {
		return nil, err
	}
	note := fmt.Sprintf("%sprinting %s for %s", autoTag, run.File, run.WorkOrder)
	if err := f.MRP.SetStatus(ctx, ws.ID, "busy", &note); err != nil {
		return nil, err
	}
	f.whyIdle(ws.Code, "")
	f.Log.Info("started print", "workstation", ws.Code, "work_order", run.WorkOrder, "step", job.StepID, "file", run.File, "per_bed", perBed, "est_min", side.EstSeconds/60)
	return nil, nil
}

// resumeRunning finds a step this station is already running with
// quantity still to make, and presents it as the job to print.
func (f *Farm) resumeRunning(ctx context.Context, ws *mrp.Workstation) (*jobRef, error) {
	step, wo, err := f.runningStep(ctx, ws.ID)
	if err != nil || step == nil {
		return nil, err
	}
	if remaining(wo.Quantity, step.QuantityDone) == "0" {
		return nil, nil
	}
	j := &mrp.Job{StepID: step.ID, Seq: step.Seq, Name: step.Name, Materials: step.Materials, CurrentMaterial: ws.CurrentMaterial}
	j.WorkOrder.ID, j.WorkOrder.Number, j.WorkOrder.Priority, j.WorkOrder.Quantity = wo.ID, wo.Number, wo.Priority, wo.Quantity
	j.Part.ID, j.Part.PartNumber, j.Part.Name = wo.Part.ID, wo.Part.PartNumber, wo.Part.Name
	return &jobRef{Job: j, resumed: true}, nil
}

// gcodeFor reads gcode.<profile> from the part's custom JSON.
func (f *Farm) gcodeFor(ctx context.Context, partID int64, profile string) (string, error) {
	raw, err := f.MRP.Custom(ctx, "parts", partID)
	if err != nil {
		return "", fmt.Errorf("part %d custom: %w", partID, err)
	}
	if len(raw) == 0 {
		return "", nil
	}
	var pc struct {
		GCode map[string]string `json:"gcode"`
	}
	if err := json.Unmarshal(raw, &pc); err != nil {
		return "", nil
	}
	return pc.GCode[profile], nil
}

// resolveGCode turns the part's G-code reference into a local file and
// its sidecar. A bare name resolves against GCodeDir; an http(s) URL is
// fetched into GCodeDir once and reused. The sidecar is optional —
// without it the run is one per bed and the window check can't see the
// duration, so it is treated as fitting.
func (f *Farm) resolveGCode(name string) (string, Sidecar, error) {
	var side Sidecar
	var path string
	switch {
	case strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://"):
		local := filepath.Join(f.Cfg.GCodeDir, filepath.Base(name))
		if _, err := os.Stat(local); err != nil {
			if err := fetch(name, local); err != nil {
				return "", side, err
			}
			_ = fetch(strings.TrimSuffix(name, ".bgcode")+".json", strings.TrimSuffix(local, ".bgcode")+".json") // best effort
		}
		path = local
	case filepath.IsAbs(name):
		path = name
	default:
		path = filepath.Join(f.Cfg.GCodeDir, name)
	}
	if _, err := os.Stat(path); err != nil {
		return "", side, err
	}
	if b, err := os.ReadFile(strings.TrimSuffix(path, filepath.Ext(path)) + ".json"); err == nil {
		_ = json.Unmarshal(b, &side)
	}
	return path, side, nil
}

func fetch(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: http %d", url, resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return err
	}
	out.Close()
	return os.Rename(tmp, dest)
}

// inWindow applies the per-printer operating window. With
// must_finish_in_window, a run must END before the window closes.
func (f *Farm) inWindow(st Settings, estSeconds int) (bool, string) {
	w := st.Window
	if w.Start == "" || w.End == "" {
		return true, ""
	}
	loc := time.Local
	if w.Timezone != "" {
		if l, err := time.LoadLocation(w.Timezone); err == nil {
			loc = l
		}
	}
	now := f.clock().In(loc)
	parse := func(s string) (time.Time, bool) {
		t, err := time.ParseInLocation("15:04", s, loc)
		if err != nil {
			return time.Time{}, false
		}
		return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc), true
	}
	start, ok1 := parse(w.Start)
	end, ok2 := parse(w.End)
	if !ok1 || !ok2 {
		return true, ""
	}
	if now.Before(start) {
		return false, "before " + w.Start
	}
	finish := now
	if w.MustFinishInWindow {
		finish = now.Add(time.Duration(estSeconds) * time.Second)
	}
	if finish.After(end) {
		if w.MustFinishInWindow {
			return false, fmt.Sprintf("would finish at %s, after %s", finish.Format("15:04"), w.End)
		}
		return false, "after " + w.End
	}
	return true, ""
}

// completeFinishedRun finds the running step on this station and
// completes it for one bed's worth: the part's per_bed from its custom
// JSON when set, else the remaining quantity. Idempotent per PrusaLink
// job id via the workstation's custom JSON.
func (f *Farm) completeFinishedRun(ctx context.Context, p *Printer, ws *mrp.Workstation, jobID int) error {
	step, wo, err := f.runningStep(ctx, ws.ID)
	if err != nil {
		return err
	}
	run := p.Settings.Run
	// Whatever happens below, the bed has been cleared: stop asking, and
	// the run — if there was one — is over.
	p.Settings.AwaitingClearJob = 0
	p.Settings.LastCompletedJob = jobID
	p.Settings.Run = nil
	if err := f.MRP.SetCustom(ctx, "workstations", ws.ID, p.Settings); err != nil {
		return err
	}
	if step == nil {
		f.Log.Info("bed cleared but no running step on this station; nothing to complete", "workstation", ws.Code, "job", jobID)
		return nil
	}
	if run != nil && run.StepID != step.ID {
		f.Log.Warn("running step is not the one this daemon started; completing it at standard", "workstation", ws.Code, "started", run.StepID, "running", step.ID)
		run = nil
	}

	// One bed's worth: the sidecar's per_bed, capped at what remains.
	qty := remaining(wo.Quantity, step.QuantityDone)
	perBed := ""
	if run != nil {
		perBed = run.PerBed
	} else if raw, err := f.MRP.Custom(ctx, "parts", wo.Part.ID); err == nil && len(raw) > 0 {
		var pc struct {
			PerBed json.Number `json:"per_bed"`
		}
		if json.Unmarshal(raw, &pc) == nil {
			perBed = pc.PerBed.String()
		}
	}
	if pb, ok := new(big.Rat).SetString(perBed); ok && pb.Sign() > 0 {
		if r, ok := new(big.Rat).SetString(qty); ok && pb.Cmp(r) < 0 {
			qty = trim(pb.FloatString(4))
		}
	}

	// Actuals: what the slicer said one bed of this file uses, in kg,
	// against the material that was loaded. Standards plan, actuals record.
	var consumed []mrp.Consumed
	if run != nil && run.FilamentPartID != 0 && run.FilamentG > 0 {
		consumed = append(consumed, mrp.Consumed{PartID: run.FilamentPartID, Quantity: fmt.Sprintf("%.4f", run.FilamentG/1000)})
	}
	if err := f.MRP.CompleteStep(ctx, step.ID, qty, consumed); err != nil {
		return fmt.Errorf("complete step %d: %w", step.ID, err)
	}
	f.Log.Info("completed step", "workstation", ws.Code, "work_order", wo.Number, "step", step.Name, "quantity", qty, "job", jobID, "actuals", consumed != nil)
	// More beds to go? Hand the step back to the queue rather than
	// keeping it: the MRP then decides whether the next bed is this
	// order again or something more urgent. A five-bed order still runs
	// straight through when nothing else is due sooner.
	if left := remaining(wo.Quantity, step.QuantityDone); left != "" {
		if r, ok := new(big.Rat).SetString(left); ok {
			if done, ok := new(big.Rat).SetString(qty); ok && r.Cmp(done) > 0 {
				if err := f.MRP.PauseStep(ctx, step.ID); err != nil {
					f.Log.Warn("pause step", "workstation", ws.Code, "step", step.ID, "err", err)
				} else {
					f.Log.Info("step paused between beds", "workstation", ws.Code, "work_order", wo.Number, "left", trim(new(big.Rat).Sub(r, done).FloatString(4)))
				}
			}
		}
	}
	return f.markIdle(ctx, ws, "bed cleared")
}

// scrapFinishedRun is the "Print failed" button: the run's filament is
// consumed at the slicer's figure (or the BOM standard when we have no
// run), nothing is produced, and the step stays open so the next idle
// tick prints the bed again.
func (f *Farm) scrapFinishedRun(ctx context.Context, p *Printer, ws *mrp.Workstation, jobID int) error {
	step, wo, err := f.runningStep(ctx, ws.ID)
	if err != nil {
		return err
	}
	run := p.Settings.Run
	p.Settings.AwaitingClearJob = 0
	p.Settings.LastCompletedJob = jobID
	p.Settings.Run = nil
	if err := f.MRP.SetCustom(ctx, "workstations", ws.ID, p.Settings); err != nil {
		return err
	}
	if step == nil {
		f.Log.Info("print reported failed but no running step on this station; nothing to scrap", "workstation", ws.Code, "job", jobID)
		return f.markIdle(ctx, ws, "bed cleared after a failed print")
	}
	qty := "1"
	var consumed []mrp.Consumed
	if run != nil {
		if run.PerBed != "" {
			qty = run.PerBed
		}
		if run.FilamentPartID != 0 && run.FilamentG > 0 {
			consumed = append(consumed, mrp.Consumed{PartID: run.FilamentPartID, Quantity: fmt.Sprintf("%.4f", run.FilamentG/1000)})
		}
	}
	if r, ok := new(big.Rat).SetString(remaining(wo.Quantity, step.QuantityDone)); ok {
		if q, ok := new(big.Rat).SetString(qty); ok && r.Cmp(q) < 0 {
			qty = trim(r.FloatString(4))
		}
	}
	if err := f.MRP.ScrapStep(ctx, step.ID, qty, consumed, "Print failed (reported at the printer)"); err != nil {
		return fmt.Errorf("scrap step %d: %w", step.ID, err)
	}
	f.Log.Info("scrapped run", "workstation", ws.Code, "work_order", wo.Number, "step", step.Name, "quantity", qty, "job", jobID)
	return f.markIdle(ctx, ws, "bed cleared after a failed print")
}

// markIdle hands the station back after the operator's button, so the next
// tick can start work. A status the operator already set to idle stands.
func (f *Farm) markIdle(ctx context.Context, ws *mrp.Workstation, why string) error {
	if ws.Status == "idle" {
		return nil
	}
	note := autoTag + why
	return f.MRP.SetStatus(ctx, ws.ID, "idle", &note)
}

// runningStep is the step in progress on this station, if any.
func (f *Farm) runningStep(ctx context.Context, wsID int64) (*mrp.Step, *mrp.WorkOrder, error) {
	orders, err := f.MRP.WorkOrders(ctx, "in_progress")
	if err != nil {
		return nil, nil, err
	}
	for _, o := range orders {
		full, err := f.MRP.WorkOrder(ctx, o.ID)
		if err != nil {
			return nil, nil, err
		}
		for i := range full.Steps {
			s := &full.Steps[i]
			if s.Status == "running" && s.WorkstationID != nil && *s.WorkstationID == wsID {
				return s, full, nil
			}
		}
	}
	return nil, nil, nil
}

func remaining(total, done string) string {
	t, ok1 := new(big.Rat).SetString(total)
	d, ok2 := new(big.Rat).SetString(done)
	if !ok1 || !ok2 {
		return total
	}
	r := new(big.Rat).Sub(t, d)
	if r.Sign() <= 0 {
		return "0"
	}
	return trim(r.FloatString(4))
}

func trim(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

func hms(seconds int) string {
	d := time.Duration(seconds) * time.Second
	return fmt.Sprintf("%dh%02dm left", int(d.Hours()), int(d.Minutes())%60)
}
