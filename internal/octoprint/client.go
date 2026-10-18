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
// Package octoprint is a small client for the OctoPrint REST API
// (verified against OctoPrint 1.11.8 and its virtual printer), and a
// driver that presents it in the daemon's printer vocabulary. Reads are
// plain GETs with an X-Api-Key header; the one write the daemon needs,
// upload-and-print, is kept obvious.
//
// No printer state is cached here. Every call asks OctoPrint; the
// daemon's loop decides how often.
package octoprint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one OctoPrint. Safe for concurrent use: it holds no
// per-call state, and http.Client is itself concurrent-safe.
type Client struct {
	base   string // "http://octopi.local", no trailing slash
	key    string
	http   *http.Client
	upload *http.Client // long timeout, see New
}

// Error is a non-2xx answer from OctoPrint, kept whole so a caller can
// log it.
type Error struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("octoprint: %s %s: http %d: %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

var (
	// ErrUnauthorized is a missing or wrong API key. OctoPrint answers
	// 403 for both, and 401 on some proxies in front of it.
	ErrUnauthorized = errors.New("octoprint: unauthorized (check the API key)")
	// ErrNotOperational is OctoPrint running but not connected to its
	// printer: unplugged, powered off, or never connected.
	ErrNotOperational = errors.New("octoprint: not connected to the printer")
)

// uploadTimeout bounds one file transfer, as in prusalink: generous,
// because OctoPrint answers only once the whole file is stored.
const uploadTimeout = 15 * time.Minute

// New returns a client for the OctoPrint at baseURL.
func New(baseURL, apiKey string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("octoprint: bad URL %q (want http://host)", baseURL)
	}
	if apiKey == "" {
		return nil, errors.New("octoprint: empty API key")
	}
	return &Client{
		base:   strings.TrimRight(u.String(), "/"),
		key:    apiKey,
		http:   &http.Client{Timeout: 10 * time.Second},
		upload: &http.Client{Timeout: uploadTimeout},
	}, nil
}

// send authenticates req, sends it with hc, and decodes a JSON answer
// into out (which may be nil).
func (c *Client) send(hc *http.Client, req *http.Request, out any) error {
	method, path := req.Method, req.URL.Path
	req.Header.Set("X-Api-Key", c.key)

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("octoprint: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		// Bounded read: an error page should not become a memory problem.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &Error{Status: resp.StatusCode, Method: method, Path: path, Body: string(b)}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("octoprint: decode %s %s: %w", method, path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("octoprint: build GET %s: %w", path, err)
	}
	return c.send(c.http, req, out)
}

// Version is GET /api/version.
func (c *Client) Version(ctx context.Context) (*Version, error) {
	var v Version
	if err := c.get(ctx, "/api/version", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Printer is GET /api/printer without temperatures and SD state, which
// the daemon doesn't use. ErrNotOperational when OctoPrint has no
// printer connected (it answers 409).
func (c *Client) Printer(ctx context.Context) (*Printer, error) {
	var p Printer
	err := c.get(ctx, "/api/printer?exclude=temperature,sd", &p)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusConflict {
		return nil, ErrNotOperational
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Job is GET /api/job, answered whether or not a printer is connected.
func (c *Client) Job(ctx context.Context) (*Job, error) {
	var j Job
	if err := c.get(ctx, "/api/job", &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// File is GET /api/files/{origin}/{path}, for a file's print history.
func (c *Client) File(ctx context.Context, origin, path string) (*File, error) {
	var f File
	if err := c.get(ctx, filePath(origin, path), &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// Delete removes a file and, with it, its print history. Deleting a file
// that isn't there is not an error; deleting one that is printing is
// (OctoPrint answers 409).
func (c *Client) Delete(ctx context.Context, origin, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+filePath(origin, path), nil)
	if err != nil {
		return fmt.Errorf("octoprint: build DELETE %s: %w", path, err)
	}
	err = c.send(c.http, req, nil)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return nil
	}
	return err
}

func filePath(origin, path string) string {
	var segs []string
	for _, s := range strings.Split(path, "/") {
		segs = append(segs, url.PathEscape(s))
	}
	return "/api/files/" + url.PathEscape(origin) + "/" + strings.Join(segs, "/")
}

// Upload stores a file in OctoPrint's local storage under name, selects
// it, and starts printing it when printNow is set. An existing file of
// that name is deleted first, not overwritten: an overwrite keeps the
// old file's print history, and the driver reads "this file has been
// printed" as "a print has ended" (see Driver.Status).
//
// The body is a multipart form, streamed: the form's header and footer
// are small and built in memory, the file itself is read from body as
// it is sent. Knowing all three lengths lets us send an exact
// Content-Length rather than a chunked body, which is what a large
// upload over a weak link should be.
func (c *Client) Upload(ctx context.Context, name string, body io.Reader, size int64, printNow bool) error {
	if strings.ContainsAny(name, "/\\") || name == "" {
		return fmt.Errorf("octoprint: bad upload name %q", name)
	}
	if err := c.Delete(ctx, "local", name); err != nil {
		return fmt.Errorf("octoprint: clear %s before upload: %w", name, err)
	}
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if err := mw.WriteField("select", "true"); err != nil {
		return err
	}
	if err := mw.WriteField("print", strconv.FormatBool(printNow)); err != nil {
		return err
	}
	if _, err := mw.CreateFormFile("file", name); err != nil {
		return err
	}
	// Close writes the closing boundary after whatever is in head so far.
	// Split it off: it is the footer that goes after the file's bytes.
	n := head.Len()
	if err := mw.Close(); err != nil {
		return err
	}
	tail := bytes.Clone(head.Bytes()[n:])
	head.Truncate(n)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/files/local",
		io.MultiReader(&head, io.LimitReader(body, size), bytes.NewReader(tail)))
	if err != nil {
		return fmt.Errorf("octoprint: build upload %s: %w", name, err)
	}
	req.ContentLength = int64(head.Len()) + size + int64(len(tail))
	req.Header.Set("Content-Type", mw.FormDataContentType())

	var up Uploaded
	if err := c.send(c.upload, req, &up); err != nil {
		return err
	}
	// OctoPrint stores the file but quietly declines to print it when the
	// printer can't (disconnected, already printing). Say so, or the
	// daemon would believe a print is running.
	if printNow && !up.EffectivePrint {
		return fmt.Errorf("octoprint: stored %s but the printer did not start it", name)
	}
	return nil
}
