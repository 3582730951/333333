// Package tokensave applies conservative, content-agnostic compression to large text
// blocks flowing through the relay (chiefly tool results the downstream agent feeds
// back into the conversation), to cut the input tokens billed by the upstream — the
// server-side analogue of rtk ("Rust Token Killer"), which compresses command output
// before it reaches the model.
//
// A relay cannot know which command produced a given tool result, so this ports only
// rtk's GENERIC, command-agnostic transforms (its truncation caps + cleanup), never the
// per-command semantic filters. Every transform is conservative and idempotent: small
// blocks are returned unchanged, and truncation keeps the head AND tail with an explicit
// elision marker so the model still sees how a long output began and ended. The feature
// is opt-in (default OFF) because mutating request content can affect model output.
package tokensave

import (
	"fmt"
	"regexp"
	"strings"
)

// ansiCSI matches ANSI CSI escape sequences (colors, cursor moves) that bloat tool
// output without carrying meaning for the model.
var ansiCSI = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// Options controls the compression. The zero value is a no-op (nothing enabled / no
// caps), so callers opt into each transform explicitly; DefaultOptions is the tuned set.
type Options struct {
	StripANSI     bool // remove ANSI CSI escape sequences
	TrimTrailing  bool // strip trailing whitespace on each line
	CollapseBlank bool // collapse >1 consecutive blank lines to a single blank line
	CollapseDupes int  // collapse runs of N+ identical lines to one + a "[repeated]" marker (0 = off)
	MaxLines      int  // when the block exceeds this many lines, keep HeadLines + TailLines (0 = no cap)
	HeadLines     int
	TailLines     int
}

// DefaultOptions is a deliberately conservative tuning: only blocks over ~400 lines are
// truncated (keeping 200 head + 120 tail), and only runs of 5+ identical lines collapse.
func DefaultOptions() Options {
	return Options{
		StripANSI:     true,
		TrimTrailing:  true,
		CollapseBlank: true,
		CollapseDupes: 5,
		MaxLines:      400,
		HeadLines:     200,
		TailLines:     120,
	}
}

// Compress applies the options to s and returns the result. It is idempotent and safe to
// call on any text: with the default options a block under the line cap only gets cheap
// cleanup (ANSI/whitespace/dupes), and is returned unchanged when there is nothing to do.
func Compress(s string, o Options) string {
	if s == "" {
		return s
	}
	if o.StripANSI {
		s = ansiCSI.ReplaceAllString(s, "")
	}
	// Preserve a trailing newline across the line round-trip.
	trailingNL := strings.HasSuffix(s, "\n")
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")

	if o.TrimTrailing {
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " \t\r")
		}
	}
	if o.CollapseBlank {
		lines = collapseBlankLines(lines)
	}
	if o.CollapseDupes > 1 {
		lines = collapseDupeLines(lines, o.CollapseDupes)
	}
	if o.MaxLines > 0 && len(lines) > o.MaxLines && o.HeadLines+o.TailLines < len(lines) {
		head := o.HeadLines
		tail := o.TailLines
		elided := len(lines) - head - tail
		out := make([]string, 0, head+tail+1)
		out = append(out, lines[:head]...)
		out = append(out, fmt.Sprintf("… [%d lines elided by token-saver] …", elided))
		out = append(out, lines[len(lines)-tail:]...)
		lines = out
	}

	res := strings.Join(lines, "\n")
	if trailingNL {
		res += "\n"
	}
	return res
}

func collapseBlankLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	blank := false
	for _, ln := range lines {
		isBlank := strings.TrimSpace(ln) == ""
		if isBlank && blank {
			continue
		}
		blank = isBlank
		out = append(out, ln)
	}
	return out
}

func collapseDupeLines(lines []string, threshold int) []string {
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		run := j - i
		if run >= threshold {
			out = append(out, lines[i], fmt.Sprintf("… [previous line repeated %d times] …", run-1))
		} else {
			for k := i; k < j; k++ {
				out = append(out, lines[k])
			}
		}
		i = j
	}
	return out
}
