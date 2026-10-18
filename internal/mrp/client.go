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
// Package mrp is the daemon's client for the Foudra MRP API.
//
// Decimals are strings on the wire and stay strings here: quantities
// and money are never floats. Never. Floats.
//
package mrp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	base string
	key  string
	http *http.Client
}

type Error struct {
	Status int
	Method string
	Path   string
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("mrp: %s %s: http %d: %s", e.Method, e.Path, e.Status, e.Msg)
}

var (
	ErrUnauthorized = errors.New("mrp: unauthorized (check MRP_API_KEY)")
	ErrNotFound     = errors.New("mrp: not found")
)

func New(baseURL, apiKey string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("mrp: bad MRP_URL %q", baseURL)
	}
	if apiKey == "" {
		return nil, errors.New("mrp: empty API key")
	}
	return &Client{
		base: strings.TrimRight(u.String(), "/"),
		key:  apiKey,
		http: &http.Client{Timeout: 20 * time.Second},
	}, nil
}

// do sends JSON in (may be nil) and decodes JSON out (may be nil). The
// MRP wraps reads in {"data": …}; callers pass the envelope type.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("mrp: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("mrp: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mrp: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &Error{Status: resp.StatusCode, Method: method, Path: path, Msg: e.Error}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("mrp: decode %s %s: %w", method, path, err)
	}
	return nil
}

// --- Workstations ---------------------------------------------------------

type MaterialRef struct {
	PartID     int64   `json:"part_id"`
	PartNumber *string `json:"part_number"`
	Name       *string `json:"name"`
}

type Workstation struct {
	ID    int64  `json:"id"`
	Code  string `json:"code"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Group struct {
		ID   int64  `json:"id"`
		Code string `json:"code"`
		Name string `json:"name"`
	} `json:"group"`
	Status          string       `json:"status"`
	StatusNote      *string      `json:"status_note"`
	IsActive        bool         `json:"is_active"`
	CurrentMaterial *MaterialRef `json:"current_material"`
}

func (c *Client) Workstations(ctx context.Context) ([]Workstation, error) {
	var env struct {
		Data []Workstation `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/workstations", nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

func (c *Client) Workstation(ctx context.Context, id int64) (*Workstation, error) {
	var env struct {
		Data Workstation `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/workstations/%d", id), nil, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// SetStatus mirrors a machine's state: idle, busy, blocked, offline.
func (c *Client) SetStatus(ctx context.Context, id int64, status string, note *string) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/api/workstations/%d/status", id),
		map[string]any{"status": status, "note": note}, nil)
}

// SetMaterial records what is loaded, or clears it with nil.
func (c *Client) SetMaterial(ctx context.Context, id int64, partID *int64) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/api/workstations/%d/material", id),
		map[string]any{"part_id": partID}, nil)
}

// Custom is the opaque JSON on a workstation, part or work order —
// where the printer's address, profile and window live.
func (c *Client) Custom(ctx context.Context, kind string, id int64) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/%s/%d/custom", kind, id), nil, &raw); err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	return raw, nil
}

func (c *Client) SetCustom(ctx context.Context, kind string, id int64, v any) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/api/%s/%d/custom", kind, id), v, nil)
}

// --- Jobs and steps -------------------------------------------------------

type Material struct {
	PartID     int64  `json:"part_id"`
	PartNumber string `json:"part_number"`
	Name       string `json:"name"`
	UOM        string `json:"unit_of_measure"`
	QtyPer     string `json:"qty_per"`
	Required   string `json:"required"`
	OnHand     string `json:"on_hand"`
	Short      bool   `json:"short"`
}

// Job is what GET /api/workstations/{id}/next hands a station.
type Job struct {
	StepID    int64  `json:"step_id"`
	Seq       int    `json:"seq"`
	Name      string `json:"name"`
	WorkOrder struct {
		ID       int64  `json:"id"`
		Number   string `json:"number"`
		Priority int    `json:"priority"`
		Quantity string `json:"quantity"`
	} `json:"work_order"`
	Part struct {
		ID         int64   `json:"id"`
		PartNumber *string `json:"part_number"`
		Name       string  `json:"name"`
	} `json:"part"`
	Materials              []Material   `json:"materials"`
	CurrentMaterial        *MaterialRef `json:"current_material"`
	RequiresMaterialChange bool         `json:"requires_material_change"`
}

// Next asks what the station should do now. material restricts to jobs
// consuming that part ("anything ready in what I have loaded?"). Nil
// when nothing is ready or the station is not idle.
func (c *Client) Next(ctx context.Context, wsID int64, material *int64) (*Job, error) {
	path := fmt.Sprintf("/api/workstations/%d/next", wsID)
	if material != nil {
		path += fmt.Sprintf("?material=%d", *material)
	}
	var env struct {
		Data *Job `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

type Step struct {
	ID            int64      `json:"id"`
	Seq           int        `json:"seq"`
	Name          string     `json:"name"`
	Status        string     `json:"status"`
	WorkstationID *int64     `json:"workstation_id"`
	QuantityDone  string     `json:"quantity_done"`
	Ready         bool       `json:"ready"`
	Materials     []Material `json:"materials"`
}

type WorkOrder struct {
	ID       int64  `json:"id"`
	Number   string `json:"number"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
	Part     struct {
		ID         int64   `json:"id"`
		PartNumber *string `json:"part_number"`
		Name       string  `json:"name"`
	} `json:"part"`
	Quantity     string `json:"quantity"`
	QuantityDone string `json:"quantity_done"`
	Steps        []Step `json:"steps"`
}

func (c *Client) WorkOrders(ctx context.Context, status string) ([]WorkOrder, error) {
	path := "/api/work-orders"
	if status != "" {
		path += "?status=" + url.QueryEscape(status)
	}
	var env struct {
		Data []WorkOrder `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

func (c *Client) WorkOrder(ctx context.Context, id int64) (*WorkOrder, error) {
	var env struct {
		Data WorkOrder `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/work-orders/%d", id), nil, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// Consumed is an actual: part id and decimal quantity.
type Consumed struct {
	PartID   int64  `json:"part_id"`
	Quantity string `json:"quantity"`
}

func (c *Client) StartStep(ctx context.Context, stepID, wsID int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/work-order-steps/%d/start", stepID),
		map[string]any{"workstation_id": wsID}, nil)
}

// CompleteStep reports units made, with actuals where known. Standards
// plan, actuals record.
func (c *Client) CompleteStep(ctx context.Context, stepID int64, qty string, consumed []Consumed) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/work-order-steps/%d/complete", stepID),
		map[string]any{"quantity": qty, "consumed": consumed}, nil)
}

// PauseStep puts a step with quantity left back in the queue, progress
// kept, so the next job is chosen afresh.
func (c *Client) PauseStep(ctx context.Context, stepID int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/work-order-steps/%d/pause", stepID), nil, nil)
}

// ScrapStep reports a run that made nothing.
func (c *Client) ScrapStep(ctx context.Context, stepID int64, qty string, consumed []Consumed, note string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/work-order-steps/%d/scrap", stepID),
		map[string]any{"quantity": qty, "consumed": consumed, "note": note}, nil)
}

// --- Tasks ----------------------------------------------------------------

type Task struct {
	Key      string   `json:"key"`
	Priority int      `json:"priority"`
	Message  string   `json:"message"`
	Href     string   `json:"href"`
	Actions  []Action `json:"actions,omitempty"`
}

// Action is a button on a task. Pressing it resolves the task with Key
// as the outcome, which TaskStates hands back.
type Action struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Tone  string `json:"tone,omitempty"` // "primary" | "danger" | ""
}

// TaskState is a task as the MRP reports it back: open, or resolved
// with the outcome an operator chose.
type TaskState struct {
	ID         int64   `json:"id"`
	Key        string  `json:"key"`
	Outcome    *string `json:"outcome"`
	ResolvedAt *string `json:"resolved_at"`
}

// TaskStates reads the source's open tasks and its recently resolved
// ones with an outcome — how the daemon learns which button was pressed.
func (c *Client) TaskStates(ctx context.Context, source string) ([]TaskState, error) {
	var env struct {
		Data []TaskState `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/tasks/"+url.PathEscape(source), nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// ReplaceTasks makes the source's open set exactly tasks: anything not
// listed is resolved. Push the whole set every time; a restart
// self-heals.
func (c *Client) ReplaceTasks(ctx context.Context, source string, tasks []Task) error {
	if tasks == nil {
		tasks = []Task{}
	}
	return c.do(ctx, http.MethodPut, "/api/tasks/"+url.PathEscape(source), tasks, nil)
}
