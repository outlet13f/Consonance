package httpapi

import (
	"strings"
	"testing"

	"github.com/pb33f/libopenapi"
)

// The served document must build as an OpenAPI 3.1 model: libopenapi resolves every $ref and
// reports what it cannot read.
func TestOpenAPIIsValid31(t *testing.T) {
	doc, err := libopenapi.NewDocument(openAPIDocument)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	model, errs := doc.BuildV3Model()
	if errs != nil {
		t.Errorf("BuildV3Model: %v", errs)
	}
	if model == nil {
		t.Fatal("no model")
	}
	if v := model.Model.Version; !strings.HasPrefix(v, "3.1") {
		t.Errorf("version = %q, want 3.1.x", v)
	}
}
