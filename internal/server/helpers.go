package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

// writeJSONError writes `{"error": msg}` with the given status. Use it for
// everything under /api/: http.Error forces Content-Type text/plain, which
// would break the "/api/* returns JSON, including every error" contract.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readJSON decodes a request body into v, refusing unknown fields so a typo
// in a field name is an error rather than a silently ignored value.
func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}
