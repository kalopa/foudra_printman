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

// The shapes OctoPrint returns, as recorded from OctoPrint 1.11.8
// driving its virtual printer (testdata/, 2026-10-07). As in prusalink,
// fields we don't use are left out: a leaner struct is a smaller
// contract with a moving target.

// Version is GET /api/version.
type Version struct {
	API    string `json:"api"`
	Server string `json:"server"`
	Text   string `json:"text"`
}

// Flags are OctoPrint's yes/no view of the printer. The daemon reads
// these rather than the state text, which is for operators and varies
// ("Printing", "Printing from SD", "Offline after error: …").
type Flags struct {
	Operational   bool `json:"operational"`
	Ready         bool `json:"ready"`
	Printing      bool `json:"printing"`
	Pausing       bool `json:"pausing"`
	Paused        bool `json:"paused"`
	Resuming      bool `json:"resuming"`
	Cancelling    bool `json:"cancelling"`
	Finishing     bool `json:"finishing"`
	Error         bool `json:"error"`
	ClosedOrError bool `json:"closedOrError"`
}

// Printer is GET /api/printer, without temperatures or SD state. Only
// answered while OctoPrint is connected to the printer; otherwise 409.
type Printer struct {
	State struct {
		Text  string `json:"text"`
		Error string `json:"error"`
		Flags Flags  `json:"flags"`
	} `json:"state"`
}

// Job is GET /api/job: the selected file and the progress through it.
// A file stays selected after its print ends, and every field is null
// when nothing is selected, which decodes as zero values here.
type Job struct {
	Job struct {
		File JobFile `json:"file"`
	} `json:"job"`
	Progress struct {
		Completion    *float64 `json:"completion"`    // percent; null before a print and after a cancel
		PrintTimeLeft *int     `json:"printTimeLeft"` // seconds; null when OctoPrint can't estimate
	} `json:"progress"`
	State string `json:"state"` // the operator text, answered even when disconnected
}

// JobFile is the selected file as /api/job describes it.
type JobFile struct {
	Name    string `json:"name"`
	Display string `json:"display"`
	Origin  string `json:"origin"` // "local" or "sdcard"
	Path    string `json:"path"`
	// Date is when this file was uploaded, in Unix seconds. A new upload
	// under the same name changes it; the file's print history does not.
	Date int64 `json:"date"`
}

// File is GET /api/files/{origin}/{path}: here, only its print history.
type File struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Prints *struct {
		Success int `json:"success"`
		Failure int `json:"failure"`
		Last    *struct {
			Date    float64 `json:"date"` // when the last print ended, Unix seconds
			Success bool    `json:"success"`
		} `json:"last"`
	} `json:"prints"` // absent until the file has been printed
}

// Uploaded is the answer to POST /api/files/local.
type Uploaded struct {
	Done           bool `json:"done"`
	EffectivePrint bool `json:"effectivePrint"` // the printer really started it
}
