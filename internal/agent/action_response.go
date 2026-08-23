package agent

import (
	"encoding/json"
	"fmt"
)

const attachmentPreviewChars = 6000

type commandActionResult struct {
	Workdir              string   `json:"workdir"`
	ExitCode             int      `json:"exit_code"`
	Stdout               string   `json:"stdout"`
	Stderr               string   `json:"stderr"`
	TimedOut             bool     `json:"timed_out"`
	Truncated            bool     `json:"truncated"`
	InlineTruncated      bool     `json:"inline_truncated,omitempty"`
	DurationMS           int64    `json:"duration_ms"`
	OpenAIFileResponse   []string `json:"openaiFileResponse,omitempty"`
	FullOutputAttached   bool     `json:"full_output_attached,omitempty"`
	CaptureTruncated     bool     `json:"capture_truncated"`
	OutputFileTTLSeconds int      `json:"output_file_ttl_seconds,omitempty"`
}

func commandActionFromHost(result hostCommandResult) commandActionResult {
	return commandActionResult{
		Workdir:    result.Workdir,
		ExitCode:   result.ExitCode,
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		TimedOut:   result.TimedOut,
		Truncated:  result.Truncated,
		DurationMS: result.DurationMS,
	}
}

func actionJSONFits(value any) bool {
	body, err := json.Marshal(value)
	return err == nil && len(body) < maxActionJSONBytes
}

func (s *Server) registerCapturedOutput(origin string, stdout, stderr *cappedBuffer, deleteOnExpiry bool) ([]string, bool, error) {
	type streamCapture struct {
		name  string
		parts []capturePart
		trunc bool
	}
	stdoutParts, stdoutTruncated := stdout.capturedParts()
	stderrParts, stderrTruncated := stderr.capturedParts()
	streams := []streamCapture{
		{name: "stdout", parts: stdoutParts, trunc: stdoutTruncated},
		{name: "stderr", parts: stderrParts, trunc: stderrTruncated},
	}

	urls := make([]string, 0, len(stdoutParts)+len(stderrParts))
	for _, stream := range streams {
		for i, part := range stream.parts {
			if part.Size == 0 {
				continue
			}
			token, err := s.files.register(part.Path, commandOutputFilename(stream.name, i), deleteOnExpiry)
			if err != nil {
				return urls, true, fmt.Errorf("register command output file: %w", err)
			}
			urls = append(urls, outputFileURL(origin, token))
		}
	}
	return urls, stdoutTruncated || stderrTruncated, nil
}

func (s *Server) makeCommandActionResult(origin string, result hostCommandResult, stdout, stderr *cappedBuffer) (commandActionResult, error) {
	view := commandActionFromHost(result)
	if !result.Truncated && actionJSONFits(view) {
		stdout.cleanupCapture()
		stderr.cleanupCapture()
		return view, nil
	}

	urls, captureTruncated, err := s.registerCapturedOutput(origin, stdout, stderr, true)
	if err != nil {
		stdout.cleanupCapture()
		stderr.cleanupCapture()
		return commandActionResult{}, err
	}
	view.Stdout, _ = stdout.tail(attachmentPreviewChars)
	view.Stderr, _ = stderr.tail(attachmentPreviewChars)
	view.InlineTruncated = true
	view.Truncated = captureTruncated
	view.OpenAIFileResponse = urls
	view.FullOutputAttached = len(urls) > 0 && !captureTruncated
	view.CaptureTruncated = captureTruncated
	view.OutputFileTTLSeconds = int(outputFileURLTTL.Seconds())
	return view, nil
}

func (s *Server) prepareJobActionView(origin, id string, view commandJobView) (commandJobView, error) {
	result, stdout, stderr, ok := s.jobOutputState(id)
	if !ok || stdout == nil || stderr == nil {
		return view, nil
	}

	if result == nil {
		if actionJSONFits(view) {
			return view, nil
		}
		view.Stdout, _ = stdout.tail(attachmentPreviewChars)
		view.Stderr, _ = stderr.tail(attachmentPreviewChars)
		view.InlineTruncated = true
		view.Truncated = true
		return view, nil
	}

	// Terminal jobs should return the complete inline output whenever it safely
	// fits in the Action payload, even if the long-poll request used a small tail.
	view.Stdout = result.Stdout
	view.Stderr = result.Stderr
	view.Truncated = result.Truncated
	if !result.Truncated && actionJSONFits(view) {
		return view, nil
	}

	urls, captureTruncated, err := s.registerCapturedOutput(origin, stdout, stderr, false)
	if err != nil {
		return commandJobView{}, err
	}
	view.Stdout, _ = stdout.tail(attachmentPreviewChars)
	view.Stderr, _ = stderr.tail(attachmentPreviewChars)
	view.InlineTruncated = true
	view.Truncated = captureTruncated
	view.OpenAIFileResponse = urls
	view.FullOutputAttached = len(urls) > 0 && !captureTruncated
	view.CaptureTruncated = captureTruncated
	view.OutputFileTTLSeconds = int(outputFileURLTTL.Seconds())
	return view, nil
}
