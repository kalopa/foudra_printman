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

// The shapes PrusaLink returns, as verified against a Core One on
// firmware 6.8.1 (API 2.0.0). Fields we don't use are left out on
// purpose: encoding/json ignores unknown keys, so a leaner struct is a
// smaller contract with a moving target.

// State is the printer's state string. The API sends upper-case words;
// these are the ones the daemon reasons about.
type State string

const (
	StateIdle      State = "IDLE"
	StateReady     State = "READY"
	StateBusy      State = "BUSY"
	StatePrinting  State = "PRINTING"
	StatePaused    State = "PAUSED"
	StateFinished  State = "FINISHED"
	StateStopped   State = "STOPPED"
	StateError     State = "ERROR"
	StateAttention State = "ATTENTION"
)

// Version is GET /api/version.
type Version struct {
	API            string  `json:"api"`
	Server         string  `json:"server"`
	Firmware       string  `json:"firmware"`
	Printer        string  `json:"printer"`
	Hostname       string  `json:"hostname"`
	NozzleDiameter float64 `json:"nozzle_diameter"`
	Capabilities   struct {
		UploadByPut bool `json:"upload-by-put"`
	} `json:"capabilities"`
}

// Info is GET /api/v1/info. Note what is NOT here: the loaded filament.
type Info struct {
	NozzleDiameter   float64 `json:"nozzle_diameter"`
	MMU              bool    `json:"mmu"`
	Serial           string  `json:"serial"`
	Hostname         string  `json:"hostname"`
	MinExtrusionTemp int     `json:"min_extrusion_temp"`
}

// JobProgress is the job block inside a status, present only while a
// job exists.
type JobProgress struct {
	ID            int     `json:"id"`
	Progress      float64 `json:"progress"`       // percent
	TimeRemaining int     `json:"time_remaining"` // seconds, the printer's own live figure
	TimePrinting  int     `json:"time_printing"`  // seconds
}

// Status is GET /api/v1/status.
type Status struct {
	Job     *JobProgress `json:"job"` // nil when idle
	Storage struct {
		Path     string `json:"path"`
		Name     string `json:"name"`
		ReadOnly bool   `json:"read_only"`
	} `json:"storage"`
	Printer struct {
		State        State   `json:"state"`
		TempBed      float64 `json:"temp_bed"`
		TargetBed    float64 `json:"target_bed"`
		TempNozzle   float64 `json:"temp_nozzle"`
		TargetNozzle float64 `json:"target_nozzle"`
		AxisZ        float64 `json:"axis_z"`
		Flow         int     `json:"flow"`
		Speed        int     `json:"speed"`
		FanHotend    int     `json:"fan_hotend"`
		FanPrint     int     `json:"fan_print"`
	} `json:"printer"`
}

// Job is GET /api/v1/job: the running job and its file. The API answers
// 204 No Content when nothing is printing, which Client.Job reports as
// a nil *Job.
type Job struct {
	ID            int     `json:"id"`
	State         State   `json:"state"`
	Progress      float64 `json:"progress"`
	TimeRemaining int     `json:"time_remaining"`
	TimePrinting  int     `json:"time_printing"`
	File          struct {
		Name        string `json:"name"`         // short 8.3 name on the USB stick
		DisplayName string `json:"display_name"` // what the slicer called it
		Path        string `json:"path"`
		Size        int64  `json:"size"` // observed as 0 on 6.8.1; do not trust
		Timestamp   int64  `json:"m_timestamp"`
		Refs        struct {
			Download  string `json:"download"`
			Thumbnail string `json:"thumbnail"`
		} `json:"refs"`
	} `json:"file"`
}
