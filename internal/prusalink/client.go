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
// Package prusalink is a small client for the PrusaLink HTTP API that
// runs on Prusa printers (Buddy firmware; verified on a Core One,
// firmware 6.8.1, API 2.0.0). Reads are plain GETs with an X-Api-Key
// header; the two writes — upload-and-print, stop — are the only calls
// that can change what the printer is doing, and are kept obvious.
//
// No printer state is cached here. Every call asks the printer; the
// daemon's loop decides how often.
package prusalink

import (
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

// Client talks to one printer. Safe for concurrent use: it holds no
// per-call state, and http.Client is itself concurrent-safe.
type Client struct {
	base   string // "http://192.168.1.7", no trailing slash
	key    string
	http   *http.Client
	upload *http.Client // long timeout, see New
}

// Error is a non-2xx answer from the printer, kept whole so a caller
// can log it. ErrUnauthorized wraps the 401 case for errors.Is.
type Error struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("prusalink: %s %s: http %d: %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

var ErrUnauthorized = errors.New("prusalink: unauthorized (check the API key)")

// New returns a client for the printer at baseURL. The timeout covers
// each whole request: a printer on flaky Wi-Fi answers in 10 ms or not
// at all, and the daemon must never hang on it.
// uploadTimeout bounds one file transfer. Generous on purpose: a
// stalled upload is reported by the printer as a failed job, not by us.
const uploadTimeout = 15 * time.Minute

func New(baseURL, apiKey string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("prusalink: bad printer URL %q (want http://host)", baseURL)
	}
	if apiKey == "" {
		return nil, errors.New("prusalink: empty API key")
	}
	return &Client{
		base: strings.TrimRight(u.String(), "/"),
		key:  apiKey,
		http: &http.Client{Timeout: 10 * time.Second},
		// Uploads get their own client: PrusaLink answers only after
		// the whole file is on the USB stick, and a wing segment is
		// 5–8 MB, which is minutes on the printer's link, not seconds.
		upload: &http.Client{Timeout: uploadTimeout},
	}, nil
}

// do performs one request and decodes a JSON body into out (which may
// be nil when no body is expected). It returns (false, nil) for a 204,
// so callers can tell "nothing there" from "here it is".
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, headers map[string]string, out any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return false, fmt.Errorf("prusalink: build %s %s: %w", method, path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.send(req, out)
}

// send is the back half of do: authenticate, send, and turn the
// response into an error or a decoded body.
func (c *Client) send(req *http.Request, out any) (bool, error) {
	return c.sendWith(c.http, req, out)
}

func (c *Client) sendWith(hc *http.Client, req *http.Request, out any) (bool, error) {
	method, path := req.Method, req.URL.Path
	req.Header.Set("X-Api-Key", c.key)

	resp, err := hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("prusalink: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return false, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return false, ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		// Bounded read: an error page should not become a memory problem.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, &Error{Status: resp.StatusCode, Method: method, Path: path, Body: string(b)}
	}
	if out == nil {
		return true, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, fmt.Errorf("prusalink: decode %s %s: %w", method, path, err)
	}
	return true, nil
}

// Version is GET /api/version.
func (c *Client) Version(ctx context.Context) (*Version, error) {
	var v Version
	if _, err := c.do(ctx, http.MethodGet, "/api/version", nil, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Info is GET /api/v1/info.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/info", nil, nil, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// Status is GET /api/v1/status: state, temperatures, and job progress
// when there is a job.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/status", nil, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Job is GET /api/v1/job. Nil, nil when nothing is printing (the API
// answers 204).
func (c *Client) Job(ctx context.Context) (*Job, error) {
	var j Job
	ok, err := c.do(ctx, http.MethodGet, "/api/v1/job", nil, nil, &j)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &j, nil
}

// --- The two calls that change what the printer is doing. ---------------

// Upload puts a file onto the printer's USB storage under name, and
// starts printing it when printNow is set. The printer advertises
// capabilities.upload-by-put; this is that. Overwrite is always allowed
// because the daemon owns the names it uploads.
//
// The caller passes the body as a reader so a large .bgcode streams
// from disk rather than sitting in memory.
func (c *Client) Upload(ctx context.Context, name string, body io.Reader, size int64, printNow bool) error {
	if strings.ContainsAny(name, "/\\") || name == "" {
		return fmt.Errorf("prusalink: bad upload name %q", name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/api/v1/files/usb/"+url.PathEscape(name), body)
	if err != nil {
		return fmt.Errorf("prusalink: build upload %s: %w", name, err)
	}
	// PrusaLink answers 411 Length Required to a chunked body, and Go
	// only knows the length of a bytes/strings reader on its own — a
	// file body would be sent chunked. So the caller says how big it is.
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Overwrite", "?1")
	if printNow {
		req.Header.Set("Print-After-Upload", "?1")
	}
	_, err = c.sendWith(c.upload, req, nil)
	return err
}

// Stop cancels job id. Job ids are the integers PrusaLink reports in
// Status.Job.ID / Job.ID; stopping a job that is no longer current is
// the printer's error to report, not ours to guess about.
func (c *Client) Stop(ctx context.Context, jobID int) error {
	_, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/job/%d/stop", jobID), nil, nil, nil)
	return err
}
