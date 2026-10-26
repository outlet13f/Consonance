package agentapi_test

import (
	"context"
	"strings"
	"testing"

	"consonance/internal/agentapi"
)

func TestValidBranchName(t *testing.T) {
	for _, name := range []string{"main", "dev-vibe-1", "a", "0", strings.Repeat("a", 40), "main-before-20261007031500"} {
		if !agentapi.ValidBranchName(name) {
			t.Errorf("ValidBranchName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "Dev", "-dev", "dev-", "dev_1", "dev.1", "..", "../x", "a/b", "%2F", "dev 1", strings.Repeat("a", 41)} {
		if agentapi.ValidBranchName(name) {
			t.Errorf("ValidBranchName(%q) = true", name)
		}
	}
}

func TestValidRestorePointID(t *testing.T) {
	if !agentapi.ValidRestorePointID("rp-20261007T031500Z-ab12") {
		t.Error("the spec 4.4 example is rejected")
	}
	for _, id := range []string{"", "rp-20261007T031500Z-AB12", "rp-20261007T031500-ab12", "rp-20261007T031500Z-ab1", "../rp-20261007T031500Z-ab12", "rp-20261007T031500Z-ab12/x"} {
		if agentapi.ValidRestorePointID(id) {
			t.Errorf("ValidRestorePointID(%q) = true", id)
		}
	}
}

func TestRequestIDInContext(t *testing.T) {
	if id, ok := agentapi.RequestIDFrom(context.Background()); ok || id != "" {
		t.Errorf("empty ctx: %q, %v", id, ok)
	}
	if id, ok := agentapi.RequestIDFrom(agentapi.WithRequestID(context.Background(), "r1")); !ok || id != "r1" {
		t.Errorf("RequestIDFrom = %q, %v", id, ok)
	}
	for _, bad := range []string{"", "a b", "a\nb", "a/b", strings.Repeat("a", 65)} {
		if id, ok := agentapi.RequestIDFrom(agentapi.WithRequestID(context.Background(), bad)); ok {
			t.Errorf("RequestIDFrom accepted %q as %q", bad, id)
		}
	}
	if !agentapi.ValidRequestID(strings.Repeat("a", 64)) || !agentapi.ValidRequestID("Req_1-x") {
		t.Error("a valid id is rejected")
	}
}
