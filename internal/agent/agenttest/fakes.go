// Package agenttest provides a fake volume for tests of the agent API and its clients.
package agenttest

import (
	"context"
	"sync"

	"consonance/internal/agentapi"
)

// Call is one operation the fake received.
type Call struct {
	Op     string // the method name, e.g. "CreateBranchData"
	Branch string
	ID     string // restore point ID
	N      int    // generation
	Body   any    // the request value, for operations that take one
}

// FakeOps implements agent.Ops. Every operation records its call and returns the error set with
// SetErr, or else the matching result field. Set the result fields before serving.
type FakeOps struct {
	PreflightResult   agentapi.Preflight
	UsageResult       agentapi.Usage
	GenerationResult  agentapi.Generation
	BranchDataResult  agentapi.BranchData
	RestorePoint      agentapi.RestorePoint
	RestorePoints     []agentapi.RestorePoint
	GenerationsResult agentapi.Generations
	DetachResult      agentapi.DetachResponse

	mu    sync.Mutex
	err   error
	calls []Call
}

// SetErr makes every following operation fail with err (nil: succeed again).
func (f *FakeOps) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Last returns the most recent call, or a zero Call when there was none.
func (f *FakeOps) Last() Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return Call{}
	}
	return f.calls[len(f.calls)-1]
}

func (f *FakeOps) record(c Call) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.err
}

func (f *FakeOps) Preflight() (agentapi.Preflight, error) {
	err := f.record(Call{Op: "Preflight"})
	return f.PreflightResult, err
}

func (f *FakeOps) Usage() (agentapi.Usage, error) {
	err := f.record(Call{Op: "Usage"})
	return f.UsageResult, err
}

func (f *FakeOps) InitMain(context.Context) (agentapi.Generation, error) {
	err := f.record(Call{Op: "InitMain"})
	return f.GenerationResult, err
}

func (f *FakeOps) CreateBranchData(_ context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error) {
	err := f.record(Call{Op: "CreateBranchData", Branch: name, Body: req})
	return f.BranchDataResult, err
}

func (f *FakeOps) DeleteBranchData(_ context.Context, name string) error {
	return f.record(Call{Op: "DeleteBranchData", Branch: name})
}

func (f *FakeOps) CreateRestorePoint(_ context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error) {
	err := f.record(Call{Op: "CreateRestorePoint", Branch: branch, Body: req})
	return f.RestorePoint, err
}

func (f *FakeOps) ListRestorePoints(branch string) ([]agentapi.RestorePoint, error) {
	err := f.record(Call{Op: "ListRestorePoints", Branch: branch})
	return f.RestorePoints, err
}

func (f *FakeOps) DeleteRestorePoint(_ context.Context, branch, id string) error {
	return f.record(Call{Op: "DeleteRestorePoint", Branch: branch, ID: id})
}

func (f *FakeOps) ListGenerations(branch string) (agentapi.Generations, error) {
	err := f.record(Call{Op: "ListGenerations", Branch: branch})
	return f.GenerationsResult, err
}

func (f *FakeOps) CreateGeneration(_ context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error) {
	err := f.record(Call{Op: "CreateGeneration", Branch: branch, Body: req})
	return f.GenerationResult, err
}

func (f *FakeOps) ActivateGeneration(_ context.Context, branch string, n int) (agentapi.Generations, error) {
	err := f.record(Call{Op: "ActivateGeneration", Branch: branch, N: n})
	return f.GenerationsResult, err
}

func (f *FakeOps) DetachGeneration(_ context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error) {
	err := f.record(Call{Op: "DetachGeneration", Branch: branch, N: n, Body: req})
	return f.DetachResult, err
}

func (f *FakeOps) DeleteGeneration(_ context.Context, branch string, n int) error {
	return f.record(Call{Op: "DeleteGeneration", Branch: branch, N: n})
}
