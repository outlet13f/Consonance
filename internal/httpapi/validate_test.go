package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidBranchName(t *testing.T) {
	for _, name := range []string{"dev", "a", "feature-1", strings.Repeat("a", 40), "before-20261007031500", "dev-before-2026"} {
		if err := validBranchName(name); err != nil {
			t.Errorf("validBranchName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "Dev", "-dev", "dev-", "dev_1", strings.Repeat("a", 41), "main-before-20261007031500", "x-before-12345678901234"} {
		err := validBranchName(name)
		ae, ok := err.(*apiError)
		if !ok || ae.Status != http.StatusBadRequest || ae.Type != typeValidation {
			t.Errorf("validBranchName(%q) = %v, want a 400 validation-error", name, err)
		}
	}
}

func TestValidProjectID(t *testing.T) {
	if !validProjectID("a1b2c3d4e5f6") {
		t.Error("a1b2c3d4e5f6 rejected")
	}
	for _, id := range []string{"A1b2c3d4e5f6", "a1b2c3d4e5f", "cns-a1b2c3d4e5f6", "a1b2c3d4e5f6a"} {
		if validProjectID(id) {
			t.Errorf("validProjectID(%q) = true, want false", id)
		}
	}
}

func TestDecodeBody(t *testing.T) {
	type body struct {
		Name string `json:"name"`
	}
	req := func(contentType, payload string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		return r
	}
	for name, r := range map[string]*http.Request{
		"unknown field": req("application/json", `{"nope":1}`),
		"text/plain":    req("text/plain", `{"name":"a"}`),
		"broken json":   req("application/json", `{"name":`),
		"too large":     req("application/json", `{"name":"`+strings.Repeat("a", 1<<20)+`"}`),
		"trailing data": req("application/json", `{"name":"a"} {}`),
	} {
		err := decodeBody(r, &body{})
		ae, ok := err.(*apiError)
		if !ok || ae.Status != http.StatusBadRequest || ae.Type != typeValidation {
			t.Errorf("%s: err = %v, want a 400 validation-error", name, err)
		}
	}

	for name, r := range map[string]*http.Request{
		"empty with type": req("application/json", ""),
		"empty no type":   req("", ""),
	} {
		var v body
		if err := decodeBody(r, &v); err != nil || v != (body{}) {
			t.Errorf("%s: v = %+v, err = %v, want the zero value and no error", name, v, err)
		}
	}

	var v body
	if err := decodeBody(req("application/json; charset=utf-8", `{"name":"a"}`), &v); err != nil || v.Name != "a" {
		t.Errorf("valid body: v = %+v, err = %v", v, err)
	}
	if err := decodeBody(req("", `{"name":"b"}`), &v); err != nil || v.Name != "b" {
		t.Errorf("valid body without Content-Type: v = %+v, err = %v", v, err)
	}
}
