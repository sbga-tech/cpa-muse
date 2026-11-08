package muse

import "encoding/json"

type profileError struct {
	status  int
	code    string
	message string
}

func (e *profileError) StatusCode() int { return e.status }
func (e *profileError) Code() string    { return "cpa.meta.profile." + e.code }

func (e *profileError) Error() string {
	typeName := "invalid_request_error"
	if e.status >= 500 {
		typeName = "api_error"
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]string{
		"type": typeName, "code": e.Code(), "message": e.message,
	}})
	return string(body)
}

func identityConflict() error {
	return &profileError{400, "identity_conflict", "Custom or conflicting identity headers cannot override the selected profile"}
}

func invalidProfile() error {
	return &profileError{500, "invalid_profile", "The selected profile could not be loaded or is not a valid version 1 profile"}
}

func invalidBody() error {
	return &profileError{400, "invalid_json", "The request must be one JSON object without duplicate members"}
}
