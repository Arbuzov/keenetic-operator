/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package keenetic

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// chunkReader hands out its pieces one Read at a time, so a test can put the
// prompt exactly where it wants it — including split across two reads, which is
// what a real socket does and what a naive "does this chunk contain it" check
// gets wrong.
type chunkReader struct {
	chunks []string
	err    error
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func TestReadUntilPromptStopsAtThePrompt(t *testing.T) {
	r := &chunkReader{chunks: []string{
		"KeeneticOS version 5.01\n\n",
		"(config)> ",
		"this must not be read: the next command has not been sent yet",
	}}

	var out strings.Builder
	if err := readUntilPrompt(r, &out); err != nil {
		t.Fatalf("readUntilPrompt() error = %v", err)
	}
	if strings.Contains(out.String(), "must not be read") {
		t.Errorf("read past the prompt: %q", out.String())
	}
	if !strings.Contains(out.String(), "KeeneticOS") {
		t.Errorf("banner missing from output: %q", out.String())
	}
}

// The prompt arriving in pieces is the case that matters: the router is on the
// other side of a socket and nothing guarantees it lands in one read.
func TestReadUntilPromptSpanningReads(t *testing.T) {
	r := &chunkReader{chunks: []string{"ip host nas.example.com 10.0.0.1\n(con", "fig)> "}}

	var out strings.Builder
	if err := readUntilPrompt(r, &out); err != nil {
		t.Fatalf("readUntilPrompt() error = %v", err)
	}
	if !strings.Contains(out.String(), "nas.example.com") {
		t.Errorf("output lost the config line: %q", out.String())
	}
}

// The prompt string appearing inside command output must not be taken for the
// prompt. Treating any occurrence as "the router is ready" would let the next
// command be sent early and pull the previous command's tail as its own answer —
// the same silent command/response drift this whole change exists to remove.
func TestReadUntilPromptIgnoresThePromptInsideOutput(t *testing.T) {
	r := &chunkReader{chunks: []string{
		"description entered from (config)> mode\n",
		"more output that must still be read\n",
		"(config)> ",
	}}

	var out strings.Builder
	if err := readUntilPrompt(r, &out); err != nil {
		t.Fatalf("readUntilPrompt() error = %v", err)
	}
	if !strings.Contains(out.String(), "must still be read") {
		t.Errorf("stopped at a prompt-lookalike inside the output: %q", out.String())
	}
}

// Read boundaries are arbitrary, so a chunk can end exactly on the prompt string
// while it sits in the middle of a line of output. Matching a suffix would take
// that for the prompt; matching the whole last line does not.
func TestReadUntilPromptIgnoresAChunkEndingOnThePromptText(t *testing.T) {
	r := &chunkReader{chunks: []string{
		"description entered from (config)>",
		" mode\nstill more output\n",
		"(config)> ",
	}}

	var out strings.Builder
	if err := readUntilPrompt(r, &out); err != nil {
		t.Fatalf("readUntilPrompt() error = %v", err)
	}
	if !strings.Contains(out.String(), "still more output") {
		t.Errorf("stopped at a chunk boundary that merely ended on the prompt text: %q", out.String())
	}
}

// The router paints its prompt with erase-line sequences around it, so matching
// must survive them rather than require the prompt to be the very last bytes.
func TestReadUntilPromptToleratesAnsiAfterThePrompt(t *testing.T) {
	r := &chunkReader{chunks: []string{"ip host nas.example.com 10.0.0.1\n(config)> \x1b[K"}}

	var out strings.Builder
	if err := readUntilPrompt(r, &out); err != nil {
		t.Fatalf("readUntilPrompt() error = %v", err)
	}
}

// A router that hangs up without ever prompting must surface as an error. The
// bug this whole change is about was exactly this going unnoticed: commands were
// written into a shell that was not listening, and nothing ever said so.
func TestReadUntilPromptReportsAMissingPrompt(t *testing.T) {
	boom := errors.New("connection reset")
	r := &chunkReader{chunks: []string{"KeeneticOS version 5.01\n"}, err: boom}

	var out strings.Builder
	err := readUntilPrompt(r, &out)
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want %v", err, boom)
	}
}

func TestReadUntilPromptReportsEOF(t *testing.T) {
	r := &chunkReader{chunks: []string{"no prompt here"}}

	var out strings.Builder
	if err := readUntilPrompt(r, &out); !errors.Is(err, io.EOF) {
		t.Errorf("error = %v, want io.EOF", err)
	}
}
