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
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kalopa/print_manager/internal/printer"
)

// fake replays what OctoPrint 1.11.8 answered, recorded through one
// print's whole life on its virtual printer (testdata/, 2026-10-07):
// idle, printing, paused, cancelling, finished, cancelled, re-uploaded,
// disconnected. A scenario names the fixture for each endpoint.
type fake struct {
	t       *testing.T
	key     string
	printer string // fixture for /api/printer; "" answers 409 as when disconnected
	job     string // fixture for /api/job
	file    string // fixture for /api/files/local/…; "" answers 500
	// uploaded is the upload's answer; empty means the recorded one.
	uploaded string

	form    map[string]string // the upload's form fields, file content under "file"
	clen    int64             // the upload's Content-Length
	calls   []string          // writes, in order: "DELETE name", "POST upload"
	deleted bool              // a DELETE finds nothing (404) when true
	busy    bool              // a DELETE finds the file printing (409) when true
}

func (f *fake) serve(w http.ResponseWriter, name string) {
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		f.t.Fatalf("fixture %s: %v", name, err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (f *fake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != f.key {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden) // what OctoPrint does
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/version":
			f.serve(w, "version")
		case r.Method == "GET" && r.URL.Path == "/api/printer":
			if f.printer == "" {
				w.WriteHeader(http.StatusConflict)
				f.serve(w, "printer_offline")
				return
			}
			f.serve(w, f.printer)
		case r.Method == "GET" && r.URL.Path == "/api/job":
			f.serve(w, f.job)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/files/local/"):
			if f.file == "" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			f.serve(w, f.file)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/files/local/"):
			f.calls = append(f.calls, "DELETE "+strings.TrimPrefix(r.URL.Path, "/api/files/local/"))
			switch {
			case f.busy:
				http.Error(w, `{"error":"Trying to delete a file that is currently in use"}`, http.StatusConflict)
			case f.deleted:
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			default:
				w.WriteHeader(http.StatusNoContent)
			}
		case r.Method == "POST" && r.URL.Path == "/api/files/local":
			f.calls = append(f.calls, "POST upload")
			f.clen = r.ContentLength
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.form = map[string]string{}
			for k, v := range r.MultipartForm.Value {
				f.form[k] = v[0]
			}
			if fh := r.MultipartForm.File["file"]; len(fh) == 1 {
				rd, _ := fh[0].Open()
				b, _ := io.ReadAll(rd)
				f.form["file"] = fh[0].Filename + "|" + string(b)
			}
			if f.uploaded != "" {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, f.uploaded)
				return
			}
			w.WriteHeader(http.StatusCreated)
			f.serve(w, "upload")
		default:
			http.Error(w, "no such endpoint", http.StatusNotFound)
		}
	})
}

func newDriver(t *testing.T, f *fake) Driver {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, f.key)
	if err != nil {
		t.Fatal(err)
	}
	return Driver{C: c}
}

// The whole life of a print, as the farm will see it. The case that
// matters most is the third from last: a finished print must read as
// finished, not idle, though OctoPrint itself says "Operational".
func TestDriverStatus(t *testing.T) {
	for _, tc := range []struct {
		name               string
		printer, job, file string
		want               printer.State
		raw                string
		wantJob            bool
	}{
		{"idle, nothing selected", "printer_operational", "job_none", "", printer.Idle, "Operational", false},
		{"printing", "printer_printing", "job_printing", "", printer.Printing, "Printing", true},
		{"paused", "printer_paused", "job_paused", "", printer.Paused, "Paused", true},
		{"cancelling", "printer_cancelling", "job_printing", "", printer.Busy, "Cancelling", true},
		{"finished", "printer_after_finish", "job_finished", "file_finished", printer.Finished, "Operational", true},
		{"cancelled", "printer_operational", "job_cancelled", "file_cancelled", printer.Stopped, "Operational", true},
		{"freshly uploaded by the driver", "printer_operational", "job_fresh", "file_fresh", printer.Idle, "Operational", false},
		// Uploaded over an old file by something else: the history stays,
		// so it reads as an ended print and an operator is asked.
		{"uploaded over by something else", "printer_operational", "job_after_reupload", "file_after_reupload", printer.Stopped, "Operational", true},
		{"disconnected", "", "job_offline", "", printer.Attention, "Offline", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDriver(t, &fake{t: t, key: "k", printer: tc.printer, job: tc.job, file: tc.file})
			s, err := d.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if s.State != tc.want || s.Raw != tc.raw {
				t.Errorf("state = %s (%q), want %s (%q)", s.State, s.Raw, tc.want, tc.raw)
			}
			if (s.Job != nil) != tc.wantJob {
				t.Fatalf("job = %+v, want job: %v", s.Job, tc.wantJob)
			}
			if s.Job != nil && (s.Job.ID <= 0 || s.Job.File != "drv-test.gcode") {
				t.Errorf("job = %+v", s.Job)
			}
		})
	}
}

// A finished print's id is the same on every look (so its bed is asked
// about once), and a different print of the same file gets another.
func TestDriverJobIDs(t *testing.T) {
	look := func(printer, job, file string) int {
		t.Helper()
		d := newDriver(t, &fake{t: t, key: "k", printer: printer, job: job, file: file})
		s, err := d.Status(context.Background())
		if err != nil || s.Job == nil {
			t.Fatalf("%s/%s: status %+v, err %v", job, file, s, err)
		}
		return s.Job.ID
	}
	finished := look("printer_after_finish", "job_finished", "file_finished")
	if again := look("printer_after_finish", "job_finished", "file_finished"); again != finished {
		t.Errorf("finished print's id changed between looks: %d, %d", finished, again)
	}
	if cancelled := look("printer_operational", "job_cancelled", "file_cancelled"); cancelled == finished {
		t.Errorf("the cancelled reprint has the finished print's id %d", finished)
	}
}

// Without the print history, an idle printer with a file selected is
// not known to be ready: an error, never a guess of idle.
func TestDriverWithoutHistoryIsAnError(t *testing.T) {
	d := newDriver(t, &fake{t: t, key: "k", printer: "printer_after_finish", job: "job_finished"})
	if s, err := d.Status(context.Background()); err == nil {
		t.Errorf("status = %+v, want an error", s)
	}
}

func TestUploadStreamsAMultipartForm(t *testing.T) {
	f := &fake{t: t, key: "k"}
	d := newDriver(t, f)
	body := "G28\nG1 X10\n"
	if err := d.Upload(context.Background(), "rib.gcode", strings.NewReader(body+"TRAILING"), int64(len(body)), true); err != nil {
		t.Fatal(err)
	}
	if f.form["select"] != "true" || f.form["print"] != "true" || f.form["file"] != "rib.gcode|"+body {
		t.Errorf("form = %q", f.form)
	}
	if f.clen <= int64(len(body)) {
		t.Errorf("Content-Length = %d: want an exact length, not chunked", f.clen)
	}
	if strings.Join(f.calls, ",") != "DELETE rib.gcode,POST upload" {
		t.Errorf("calls = %v: want the old file deleted before the upload", f.calls)
	}
}

// Nothing to delete is fine; a file still printing is not, and nothing
// is uploaded over it.
func TestUploadDeletesFirst(t *testing.T) {
	f := &fake{t: t, key: "k", deleted: true}
	if err := newDriver(t, f).Upload(context.Background(), "new.gcode", strings.NewReader("G28\n"), 4, true); err != nil {
		t.Errorf("first upload of a name: %v", err)
	}
	f = &fake{t: t, key: "k", busy: true}
	if err := newDriver(t, f).Upload(context.Background(), "rib.gcode", strings.NewReader("G28\n"), 4, true); err == nil {
		t.Error("uploaded over a file in use")
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %v: want only the refused DELETE", f.calls)
	}
}

// OctoPrint keeps the file but won't print when it can't; the daemon
// must hear that as a failed start, not believe a print is running.
func TestUploadThatDidNotStartIsAnError(t *testing.T) {
	d := newDriver(t, &fake{t: t, key: "k", uploaded: `{"done":true,"effectivePrint":false,"effectiveSelect":true}`})
	if err := d.Upload(context.Background(), "rib.gcode", strings.NewReader("G28\n"), 4, true); err == nil {
		t.Error("upload that did not start the print reported success")
	}
}

func TestWrongKey(t *testing.T) {
	d := newDriver(t, &fake{t: t, key: "right"})
	d.C.key = "wrong"
	if _, err := d.C.Version(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}
