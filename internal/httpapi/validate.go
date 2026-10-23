package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
)

const (
	maxBranchNameLen = 40
	maxBodyBytes     = 1 << 20
)

var (
	dns1123Label  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	backupName    = regexp.MustCompile(`^.+-before-[0-9]{14}$`)
	projectIDForm = regexp.MustCompile(`^[a-z0-9]{12}$`)
)

// validBranchName checks a branch name a user chose: a DNS-1123 label of at most 40 characters
// (it becomes part of Pod and PVC names), and not the form of the backup branches a rollback
// makes, so a user cannot pose as one.
func validBranchName(name string) error {
	switch {
	case name == "":
		return badRequest("branch name must not be empty")
	case len(name) > maxBranchNameLen:
		return badRequest("branch name %q is longer than %d characters", name, maxBranchNameLen)
	case !dns1123Label.MatchString(name):
		return badRequest("branch name %q must be lowercase letters, digits and '-', starting and ending with a letter or digit", name)
	case backupName.MatchString(name):
		return badRequest("branch name %q has the form of a backup branch (<name>-before-<14 digits>), which is reserved", name)
	}
	return nil
}

// validProjectID reports whether id has the form of a project id: 12 lowercase letters or digits.
func validProjectID(id string) bool { return projectIDForm.MatchString(id) }

// decodeBody reads the JSON body of r into v. A missing Content-Type is accepted; an empty body
// leaves v as it is. Unknown fields are refused so a misspelt field is not silently ignored.
func decodeBody(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			return badRequest("Content-Type must be application/json, not %q", ct)
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case errors.As(err, &tooLarge):
			return badRequest("body is larger than %d bytes", maxBodyBytes)
		}
		return badRequest("body is not valid JSON for this request: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequest("body has data after the JSON value")
	}
	return nil
}
