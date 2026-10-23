package v1alpha1_test

import (
	"testing"

	"consonance/api/v1alpha1"
)

func TestPermanentDataFailure(t *testing.T) {
	for reason, want := range map[string]bool{
		"ReflinkUnsupported":  true,
		"InsufficientStorage": true,
		"SourceNotFound":      true,
		"Invalid":             true,
		"ParentNotRunning":    false,
		"CopyFailed":          false,
		"":                    false,
	} {
		if got := v1alpha1.PermanentDataFailure(reason); got != want {
			t.Errorf("PermanentDataFailure(%q) = %v, want %v", reason, got, want)
		}
	}
}
