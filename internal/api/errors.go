// Package api implements corral's daemon-side HTTP surface (design doc §9):
// a ServeMux under /v1/, a version-handshake middleware, a stable error
// envelope, and the handlers for /v1/version, /v1/config, and
// /v1/daemon/shutdown. Sessions/attach endpoints land in step 9/10; this
// package only wires what step 8 needs.
package api

import (
	"encoding/json"
	"net/http"
)

// Code is one of the fixed error codes in the envelope's "error.code"
// field (design doc §9). New codes may be added in later milestones;
// existing codes are never renamed, since clients match on them.
type Code string

const (
	CodeBadRequest          Code = "bad_request"
	CodeVersionMismatch     Code = "version_mismatch"
	CodeSessionNotFound     Code = "session_not_found"
	CodeSessionNameTaken    Code = "session_name_taken"
	CodeAlreadyAttached     Code = "already_attached"
	CodeClientTooSlow       Code = "client_too_slow"
	CodeUnsupportedMode     Code = "unsupported_mode"
	CodeDaemonShuttingDown  Code = "daemon_shutting_down"
	CodeInternal            Code = "internal"
	CodeUnauthorized        Code = "unauthorized"
	CodeSessionNotLive      Code = "session_not_live"
	CodeSessionNotResumable Code = "session_not_resumable"
	CodeDAGNotFound         Code = "dag_not_found"
	CodeDAGCycle            Code = "dag_cycle"
	// CodeForbidden: token's scope='session' subtree does not include the
	// requested session/dag (m5.md §10). Distinct from CodeUnauthorized,
	// which means "no/invalid token" — this means "valid token, wrong
	// scope."
	CodeForbidden Code = "forbidden"
)

// errorBody is the "error" object inside the envelope.
type errorBody struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// errorEnvelope is the exact top-level shape of every non-2xx response
// (design doc §9): {"error":{"code":...,"message":...,"details":{}}}.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// writeError writes status and a JSON error envelope with code and
// message. details may be nil, in which case an empty object is written
// (so the envelope's shape never varies).
func writeError(w http.ResponseWriter, status int, code Code, message string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: code, Message: message, Details: details}})
}

// writeJSON writes status and v encoded as the JSON response body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
