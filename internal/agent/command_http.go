package agent

import "net/http"

func (s *Server) CommandEndpoint() http.Handler {
	return s.auth(http.HandlerFunc(s.handleRunCommand))
}

func (s *Server) handleRunCommand(w http.ResponseWriter, r *http.Request) {
	var input commandInput
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if message := s.validateCommandInput(input); message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}

	result, stdout, stderr, err := s.runHostCommandCaptured(r.Context(), input.Command, input.Workdir, input.Stdin, input.TimeoutSeconds)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	view, err := s.makeCommandActionResult(requestOrigin(r), result, stdout, stderr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}
