package main_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/testutil/pgtest"
)

const token = "agent-token"

func buildAgent(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "consonance-agent")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// newVolume returns an empty volume directory and a token file.
func newVolume(t *testing.T) (vol, tokenFile string) {
	t.Helper()
	dir := t.TempDir()
	vol = filepath.Join(dir, "volume")
	if err := os.Mkdir(vol, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile = filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return vol, tokenFile
}

// agentProc is an agent binary started by a test, with its output in a log file.
type agentProc struct {
	cmd     *exec.Cmd
	exited  chan struct{} // closed once the process has exited; waitErr is set by then
	waitErr error
	logPath string
	url     string
}

// startAgent runs the agent on vol and waits until it answers /healthz. The process is killed
// when the test ends, and its output is logged if the test failed.
func startAgent(t *testing.T, bin, vol, tokenFile string, extra ...string) *agentProc {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", pgtest.FreePort(t))
	p := &agentProc{exited: make(chan struct{}), logPath: filepath.Join(t.TempDir(), "agent.log"), url: "http://" + addr}
	logFile, err := os.Create(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	args := append([]string{"--volume", vol, "--token-file", tokenFile, "--listen", addr}, extra...)
	p.cmd = exec.Command(bin, args...)
	p.cmd.Stdout, p.cmd.Stderr = logFile, logFile
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.waitErr = p.cmd.Wait(); close(p.exited) }()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill() // nothing happens once it has exited
		<-p.exited
		if t.Failed() {
			t.Logf("agent output:\n%s", p.logs())
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get(p.url + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p
			}
		}
		select {
		case <-p.exited:
			t.Fatalf("the agent exited: %v\n%s", p.waitErr, p.logs())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent did not answer /healthz:\n%s", p.logs())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (p *agentProc) logs() string {
	b, _ := os.ReadFile(p.logPath)
	return string(b)
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestAgentServesAndShutsDown(t *testing.T) {
	bin := buildAgent(t)
	vol, tok := newVolume(t)
	p := startAgent(t, bin, vol, tok)
	ctx := testCtx(t)
	c := agentclient.New(p.url, token, nil)
	pre, err := c.Preflight(ctx)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if pre.Reflink || pre.Error == "" {
		t.Fatalf("Preflight on a temp directory = %+v, want no reflink and the reason", pre)
	}
	if _, err := agentclient.New(p.url, "wrong", nil).Usage(ctx); agentclient.CodeOf(err) != agentapi.CodeUnauthorized {
		t.Fatalf("wrong token: %v", err)
	}
	// SIGHUP, a reload habit, must not stop the agent.
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := c.Usage(ctx); err != nil {
		t.Fatalf("after SIGHUP: %v", err)
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit within 10s of SIGTERM")
	}
	if p.waitErr != nil {
		t.Fatalf("exit after SIGTERM: %v", p.waitErr)
	}
}

func TestAgentRefusesEmptyToken(t *testing.T) {
	bin := buildAgent(t)
	vol, tok := newVolume(t)
	if err := os.WriteFile(tok, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--volume", vol, "--token-file", tok, "--listen", "127.0.0.1:0").CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("the agent did not exit on its own")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("agent with an empty token: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "is empty") {
		t.Fatalf("the output does not say why:\n%s", out)
	}
}

func TestAgentRecoversOnStart(t *testing.T) {
	bin := buildAgent(t)
	vol, tok := newVolume(t)
	// A branch copy that the previous agent did not finish.
	half := filepath.Join(vol, "branches", "dev", "data-1")
	if err := os.MkdirAll(half, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(half, ".incomplete"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data-1", filepath.Join(vol, "branches", "dev", "current")); err != nil {
		t.Fatal(err)
	}
	// Complete data that must survive.
	mainData := filepath.Join(vol, "branches", "main", "data-1")
	if err := os.MkdirAll(mainData, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainData, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data-1", filepath.Join(vol, "branches", "main", "current")); err != nil {
		t.Fatal(err)
	}

	p := startAgent(t, bin, vol, tok)
	ctx := testCtx(t)
	c := agentclient.New(p.url, token, nil)
	if _, err := c.ListGenerations(ctx, "dev"); agentclient.CodeOf(err) != agentapi.CodeNotFound {
		t.Fatalf("dev after the restart: %v, want not-found", err)
	}
	if gens, err := c.ListGenerations(ctx, "main"); err != nil || gens.Current != 1 {
		t.Fatalf("main after the restart: %+v, %v", gens, err)
	}
	if !strings.Contains(p.logs(), "branches/dev") {
		t.Fatalf("the cleanup was not logged:\n%s", p.logs())
	}
	trash := filepath.Join(vol, ".consonance", "trash")
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(trash)
		if err == nil && len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the janitor did not empty the trash: %v, %v", entries, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAgentRejectsHostileNames(t *testing.T) {
	bin := buildAgent(t)
	vol, tok := newVolume(t)
	// Something next to the volume that a path trick could reach.
	outside := filepath.Join(filepath.Dir(vol), "outside")
	if err := os.MkdirAll(filepath.Join(outside, "data-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data-1", filepath.Join(outside, "current")); err != nil {
		t.Fatal(err)
	}
	p := startAgent(t, bin, vol, tok)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/v1/branches/..%2F..%2Foutside/generations"},
		{"GET", "/v1/branches/..%2F..%2Fetc/restore-points"},
		{"GET", "/v1/branches/Main/generations"},
		{"GET", "/v1/branches/" + strings.Repeat("a", 41) + "/generations"},
		{"DELETE", "/v1/branches/..%2F..%2Foutside/data"},
		{"DELETE", "/v1/branches/main/restore-points/..%2F..%2Fbranches%2Fmain"},
	} {
		req, err := http.NewRequest(rt.method, p.url+rt.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(agentapi.TokenHeader, token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s = %d %s, want 400", rt.method, rt.path, resp.StatusCode, body)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "data-1")); err != nil {
		t.Fatalf("something outside the volume was touched: %v", err)
	}
}

func TestAgentInitializesMain(t *testing.T) {
	pgBin := pgtest.BinDir(t)
	bin := buildAgent(t)
	vol, tok := newVolume(t)
	p := startAgent(t, bin, vol, tok, "--initdb", filepath.Join(pgBin, "initdb"))
	ctx := testCtx(t)
	c := agentclient.New(p.url, token, nil)
	if g, err := c.InitMain(ctx); err != nil || g.Generation != 1 {
		t.Fatalf("InitMain = %+v, %v", g, err)
	}
	if gens, err := c.ListGenerations(ctx, "main"); err != nil || gens.Current != 1 {
		t.Fatalf("ListGenerations = %+v, %v", gens, err)
	}
}
