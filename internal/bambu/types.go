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
	"bytes"
	"encoding/json"
	"strconv"
)

// GCodeState is the printer's word for what it is doing.
type GCodeState string

const (
	StateIdle    GCodeState = "IDLE"
	StatePrepare GCodeState = "PREPARE" // heating, levelling: the job has begun
	StateSlicing GCodeState = "SLICING" // a cloud print being sliced on the printer
	StateRunning GCodeState = "RUNNING"
	StatePause   GCodeState = "PAUSE"
	StateFinish  GCodeState = "FINISH" // stays until the next job starts
	StateFailed  GCodeState = "FAILED" // cancelled or faulted; stays likewise
)

// ErrCancelled is the print_error a printer reports for a job that was
// stopped from its screen or by a command, rather than one that failed.
const ErrCancelled = 0x0300400C

// Report is the printer's status, merged from its pushes. Firmware
// differs on whether ids and times are strings or numbers, so those are
// read as either.
type Report struct {
	GCodeState     GCodeState `json:"gcode_state"`
	Percent        int        `json:"mc_percent"`
	RemainingMin   int        `json:"mc_remaining_time"`
	SubtaskName    string     `json:"subtask_name"`
	GCodeFile      string     `json:"gcode_file"`
	PrintError     int        `json:"print_error"`
	TaskID         flexString `json:"task_id"`
	SubtaskID      flexString `json:"subtask_id"`
	JobID          flexString `json:"job_id"`
	GCodeStartTime flexString `json:"gcode_start_time"`
	LayerNum       int        `json:"layer_num"`
	TotalLayers    int        `json:"total_layer_num"`
	Nozzle         float64    `json:"nozzle_temper"`
	NozzleTarget   float64    `json:"nozzle_target_temper"`
	Bed            float64    `json:"bed_temper"`
	BedTarget      float64    `json:"bed_target_temper"`
	WifiSignal     string     `json:"wifi_signal"`
	HMS            []HMS      `json:"hms"`
}

// HMS is one Health Management System alert.
type HMS struct {
	Attr int64 `json:"attr"`
	Code int64 `json:"code"`
}

// flexString reads a JSON string or number as a string.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	if bytes.Equal(b, []byte("null")) {
		*f = ""
		return nil
	}
	if _, err := strconv.ParseFloat(string(b), 64); err != nil {
		return err
	}
	*f = flexString(b)
	return nil
}

// Options are the per-printer choices for starting a job, from the
// workstation's custom JSON under "bambu". All are optional.
type Options struct {
	// Plate is which plate of a multi-plate .3mf to print; default 1.
	Plate int `json:"plate,omitempty"`
	// UseAMS feeds from the AMS; AMSMapping maps the file's filaments
	// to AMS slots in order (0–3 the first AMS, 4–7 the second, ...).
	// Without the AMS the external spool is used.
	UseAMS     bool  `json:"use_ams,omitempty"`
	AMSMapping []int `json:"ams_mapping,omitempty"`
	// BedLevelling defaults to on; set false to skip it.
	BedLevelling *bool `json:"bed_levelling,omitempty"`
	Timelapse    bool  `json:"timelapse,omitempty"`
	// Storage is the URL prefix the printer opens uploaded files under.
	// Default "file:///sdcard/", which is where FTPS puts them on
	// printers with an SD card.
	Storage string `json:"storage,omitempty"`
}
