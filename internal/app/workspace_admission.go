package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/hollis-labs/substrate/harness/workspace/materialize"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

// launchScratchCustody is one admitted execution allocation. It neither grants
// launch/cleanup authority nor changes Plan.Env, CODEX_HOME or WorkRoot. This
// physical custody is distinct from the preparation decision after Start.
type launchScratchCustody struct {
	mu                sync.Mutex
	allocation        store.WorkspaceScratchAllocation
	parent, directory *os.Root
	parentInfo, info  os.FileInfo
	validateDecision  func(context.Context) error
	validateStart     func(context.Context) error
	accepted          store.WorkspaceLaunchDecision
	validateGate      func() bool
	protectedRoots    []string
	protectedInfo     []os.FileInfo
	closed            bool
}

// admitLaunchScratch narrows a held canonical launch to a generated scratch
// directory. protectedRoots must be the daemon's resolved control-plane roots,
// never caller input. Unknown reserved targets are retained and refused.
func (s *Service) admitLaunchScratch(ctx context.Context, accepted store.WorkspaceLaunchDecision, plan *launch.Plan, operation string, protectedRoots []string) (*launchScratchCustody, error) {
	row := &accepted.Session
	id := row.ID
	s.launchMu.Lock()
	gate := s.launches[id]
	var holder string
	if gate != nil {
		holder = gate.holder
	}
	s.launchMu.Unlock()
	if holder == "" {
		return nil, store.ErrWorkspaceAllocationConflict
	}
	decision, err := s.launchArtifactAdmission(ctx, id, row, plan)
	if err != nil {
		return nil, err
	}
	// Only reuse the accepted row/plan/gate predicate, not an artifact grant.
	if err := decision.Validate(ctx); err != nil {
		return nil, err
	}
	if len(protectedRoots) == 0 || !containsPath(protectedRoots, decision.ControlParent) || !scratchPathAllowed(row.Workspace, protectedRoots) {
		return nil, store.ErrWorkspaceAllocationConflict
	}
	// An existing native home is continuity state, never scratch storage.
	if plan.NativeStateRoot != "" {
		protectedRoots = append(append([]string(nil), protectedRoots...), plan.NativeStateRoot)
		if !scratchPathAllowed(filepath.Join(row.Workspace, ".tether-scratch-"+operation), protectedRoots) {
			return nil, store.ErrWorkspaceAllocationConflict
		}
	}
	var protectedInfo []os.FileInfo
	for _, root := range protectedRoots {
		info, err := scratchDirectoryInfo(root)
		if err != nil {
			return nil, err
		}
		protectedInfo = append(protectedInfo, info)
	}
	parent, parentInfo, err := openScratchDirectory(row.Workspace)
	if err != nil {
		return nil, err
	}
	allocation, replay, err := s.Store.ReservePreparedWorkspaceScratch(ctx, accepted, operation)
	if err != nil {
		_ = parent.Close()
		return nil, err
	}
	custody := &launchScratchCustody{allocation: allocation, accepted: accepted, parent: parent, parentInfo: parentInfo,
		validateDecision: decision.Validate, protectedRoots: append([]string(nil), protectedRoots...), protectedInfo: protectedInfo, validateGate: func() bool {
			s.launchMu.Lock()
			defer s.launchMu.Unlock()
			return holder != "" && s.launches[id] == gate && gate.holder == holder && len(gate.slot) == 1
		}}
	fail := func(cause error) (*launchScratchCustody, error) {
		_ = custody.Close() // Release descriptors only; retain every directory.
		return nil, cause
	}
	if replay && allocation.Status != "admitted" {
		return fail(store.ErrWorkspaceAllocationConflict)
	}
	name := filepath.Base(allocation.Root)
	if !replay {
		// Exclusive creation: existing foreign/partial directories are never
		// adopted, even when empty or named with the same operation nonce.
		if err := parent.Mkdir(name, 0o700); err != nil {
			return fail(err)
		}
	}
	custody.directory, custody.info, err = openScratchDirectory(allocation.Root)
	if err != nil {
		return fail(err)
	}
	identity := store.ScratchPhysicalIdentity{Workspace: materialize.InstalledIdentity(parentInfo), Scratch: materialize.InstalledIdentity(custody.info)}
	if identity.Workspace == "" || identity.Scratch == "" {
		return fail(store.ErrWorkspaceAllocationConflict)
	}
	if replay {
		if identity != allocation.PhysicalIdentity {
			return fail(store.ErrWorkspaceAllocationConflict)
		}
	} else {
		allocation, err = s.Store.AdmitWorkspaceScratch(ctx, allocation, identity, func(ctx context.Context) error {
			// The Store owns its sole connection here: no Store reentry.
			return custody.validatePhysical(ctx)
		})
		if err != nil {
			return fail(err)
		}
		custody.allocation = allocation
	}
	if err := custody.Validate(ctx); err != nil {
		return fail(err)
	}
	custody.validateStart = func(ctx context.Context) error {
		return s.Store.ValidateWorkspaceScratchStart(ctx, accepted, custody.allocation)
	}
	return custody, nil
}

// Validate is preparation-only: the captured created-state row must still be
// exact. Manager.Start legitimately changes state/PID/updated_at; this predicate
// is not silently refreshed to admit those changes. Runtime entry checks the
// separately permitted transition; physical custody remains independently held.
func (c *launchScratchCustody) Validate(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.validateDecision(ctx); err != nil {
		return err
	}
	return c.validatePhysical(ctx)
}

func (c *launchScratchCustody) validatePhysical(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed || !c.validateGate() || !scratchPathAllowed(c.allocation.Root, c.protectedRoots) {
		return store.ErrWorkspaceAllocationConflict
	}
	for i, root := range c.protectedRoots {
		current, err := scratchDirectoryInfo(root)
		if err != nil || !os.SameFile(c.protectedInfo[i], current) {
			return store.ErrWorkspaceAllocationConflict
		}
	}
	for _, target := range []struct {
		path string
		info os.FileInfo
	}{{filepath.Dir(c.allocation.Root), c.parentInfo}, {c.allocation.Root, c.info}} {
		current, err := scratchDirectoryInfo(target.path)
		if err != nil || !os.SameFile(target.info, current) || target.info.Mode() != current.Mode() {
			return store.ErrWorkspaceAllocationConflict
		}
	}
	if c.info.Mode().Perm() != 0o700 {
		return store.ErrWorkspaceAllocationConflict
	}
	return nil
}

// Close releases handles only. Allocation retention and deletion policy are
// deliberately separate; partial failures have no implicit rollback deletion.
func (c *launchScratchCustody) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	var errs []error
	if c.directory != nil {
		errs = append(errs, c.directory.Close())
	}
	if c.parent != nil {
		errs = append(errs, c.parent.Close())
	}
	return errors.Join(errs...)
}

func openScratchDirectory(path string) (*os.Root, os.FileInfo, error) {
	before, err := scratchDirectoryInfo(path)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	current, err := root.Stat(".")
	if err != nil || !os.SameFile(before, current) {
		_ = root.Close()
		return nil, nil, store.ErrWorkspaceAllocationConflict
	}
	return root, current, nil
}

func scratchDirectoryInfo(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, store.ErrWorkspaceAllocationConflict
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return nil, store.ErrWorkspaceAllocationConflict
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return os.Lstat(path)
}

func scratchPathAllowed(path string, protected []string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, root := range protected {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return false
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil || resolved == path || insideAnyRoot([]string{resolved}, path) || insideAnyRoot([]string{path}, resolved) {
			return false
		}
	}
	return true
}

type launchScratchKey struct {
	session, operation string
}

func (s *Service) captureLaunchScratchDecision(ctx context.Context, row *store.SessionRow, plan *launch.Plan) (store.WorkspaceLaunchDecision, error) {
	decision, err := s.Store.WorkspaceLaunchDecision(ctx, row.ID)
	if err != nil {
		return store.WorkspaceLaunchDecision{}, err
	}
	var stored launch.Plan
	if err := json.Unmarshal([]byte(decision.PlanJSON), &stored); err != nil {
		return store.WorkspaceLaunchDecision{}, err
	}
	storedVersion, err := artifactPlanVersion(&stored)
	if err != nil {
		return store.WorkspaceLaunchDecision{}, err
	}
	version, err := artifactPlanVersion(plan)
	if err != nil || version != storedVersion || !reflect.DeepEqual(decision.Session, *row) {
		return store.WorkspaceLaunchDecision{}, store.ErrWorkspaceAllocationConflict
	}
	return decision, nil
}

// launchScratchControlRoots observes existing control roots without preparing
// catalog layers or widening sandbox permissions. Missing project layers are
// lexical refusals when they contain the canonical workspace, not write effects.
func (s *Service) launchScratchControlRoots(ctx context.Context, workspaceRoot string) ([]string, error) {
	var databasePath string
	if err := s.Store.DB().QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&databasePath); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(databasePath) || s.CatalogRoot == "" {
		return nil, store.ErrWorkspaceAllocationConflict
	}
	roots := []string{filepath.Dir(databasePath), config.Expand(s.CatalogRoot)}
	if s.Catalog != nil {
		for _, root := range runDirs(s.Catalog.Global.Daemon) {
			if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, err
			}
			roots = append(roots, root)
		}
		for _, project := range s.Catalog.Projects {
			root := filepath.Join(config.Expand(project.RepoRoot), ".tether")
			if !filepath.IsAbs(root) {
				continue // An unusable unrelated project issues no effect here.
			}
			if root == workspaceRoot || pathWithin(root, workspaceRoot) || pathWithin(workspaceRoot, root) {
				return nil, store.ErrWorkspaceAllocationConflict
			}
			if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, err
			}
			roots = append(roots, root)
		}
	}
	return roots, nil
}
