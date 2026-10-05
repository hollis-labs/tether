package shimretire

import (
	"context"
	"errors"
	"os"
	"reflect"
)

var ErrCleanupUnsupported = errors.New("complete cleanup admission unavailable")

type CleanupMode uint8

const (
	DescriptorCleanup CleanupMode = iota + 1
	RetentionCleanup
)

// CleanupAttempt freezes the original evidence. The concrete adapter must bind
// Root to its own native custody; this value is not an authority capability.
type CleanupAttempt struct {
	Mode         CleanupMode
	Receipt      Receipt
	Snapshot     Snapshot
	Proof        Observation
	Artifact     Artifact
	Cursor       *SweepCursor
	Root         *os.Root
	RootIdentity FileIdentity
}

type CleanupAdmission interface {
	RemoveOwned(context.Context, CleanupAttempt) (Mutation, error)
}

// Only the owned, audited store implementation may receive effect admission.
// Concrete type provenance alone does not grant custody: that implementation
// also refuses zero/foreign construction and currently all production callers.
// Reflect examines type metadata without invoking caller methods.
func ownedAdmission(a CleanupAdmission) bool {
	t := reflect.TypeOf(a)
	return t != nil && t.Kind() == reflect.Pointer && !reflect.ValueOf(a).IsNil() && t.Elem().PkgPath() == "github.com/hollis-labs/tether/internal/store" && t.Elem().Name() == "ShimCleanupAdmission"
}
