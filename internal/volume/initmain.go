package volume

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"consonance/internal/agentapi"
)

// pgHBA is every branch's client authentication (spec 4.5): the superuser only through the
// pod-local socket with peer authentication, everyone else with SCRAM, and the superuser
// rejected over TCP. Branches inherit it with their data.
const pgHBA = `# Written by the Consonance volume agent (spec 4.5).
# TYPE  DATABASE  USER      ADDRESS  METHOD
local   all       postgres           peer
local   all       all                scram-sha-256
host    all       postgres  all      reject
host    all       all       all      scram-sha-256
`

// InitMain creates main's first generation with initdb and points current at it (spec 7.2
// POST /v1/init-main). It is the only way main's data is made. When main has data already it
// changes nothing and returns main's current generation.
func (v *Volume) InitMain(ctx context.Context) (agentapi.Generation, error) {
	release, err := v.acquire(ctx)
	if err != nil {
		return agentapi.Generation{}, err
	}
	defer release()

	n, err := v.currentGeneration(agentapi.MainBranch)
	if err == nil {
		return agentapi.Generation{Generation: n}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return agentapi.Generation{}, err // never make main again over data that could not be read
	}

	bd := v.branchDir(agentapi.MainBranch)
	if err := v.trash(bd); err != nil { // what an unfinished earlier attempt left
		return agentapi.Generation{}, err
	}
	if err := v.initMain(ctx, bd); err != nil {
		v.trashOrWarn(bd)
		return agentapi.Generation{}, err
	}
	return agentapi.Generation{Generation: 1}, nil
}

func (v *Volume) initMain(ctx context.Context, bd string) error {
	if err := os.Mkdir(bd, 0o700); err != nil {
		return err
	}
	data := filepath.Join(bd, generationName(1))
	args := append([]string{
		"-D", data,
		"-U", "postgres",
		"--auth-local=peer",
		"--auth-host=scram-sha-256",
		"--encoding=UTF8",
		"--locale-provider=builtin",
		"--locale=C.UTF-8",
		"--data-checksums",
	}, v.cfg.InitDBArgs...)
	if out, err := exec.CommandContext(ctx, v.cfg.InitDB, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("initdb: %w: %s", err, bytes.TrimSpace(out))
	}
	if err := writeFileSync(filepath.Join(data, "pg_hba.conf"), []byte(pgHBA), 0o600); err != nil {
		return err
	}
	// The link is the commit point: Recover removes a main without one.
	return switchCurrent(bd, 1)
}
