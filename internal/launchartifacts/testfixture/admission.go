// Package testfixture supplies an explicit test-owned accepted launch decision.
// Production callers use canonical daemon admission or their direct operation.
package testfixture

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/launchartifacts"
)

func Admission(t testing.TB, accepted func() any) launchartifacts.Admission {
	t.Helper()
	version, err := launchartifacts.Digest(accepted())
	if err != nil {
		t.Fatal(err)
	}
	operationCtx, closeOperation := context.WithCancel(context.Background())
	t.Cleanup(closeOperation)
	return launchartifacts.Admission{OperationID: uuid.NewString(), DecisionID: "tether.test-launch:" + t.Name(),
		Version: version, Owner: "test-owned-tether-launch", ControlParent: t.TempDir(), LocalFilesystem: true,
		Validate: func(ctx context.Context) error {
			if err := operationCtx.Err(); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			current, err := launchartifacts.Digest(accepted())
			if err != nil || current != version {
				return fmt.Errorf("test-owned accepted launch changed")
			}
			return nil
		}}
}
