package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/adapters/turn"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// ErrNativeOnlyUnavailable means the recorded native conversation could not be
// preserved. It never authorizes a fresh conversation or an automatic turn.
var ErrNativeOnlyUnavailable = errors.New("native-only resume unavailable")

func nativeOnlyError(reason string) error {
	return fmt.Errorf("%w: %w: %s", session.ErrRecoveryConflict, ErrNativeOnlyUnavailable, reason)
}

func nativeOnlyDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nativeOnlyError("recorded context directory invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nativeOnlyError("recorded context directory unavailable")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nativeOnlyError("recorded context directory ambiguous")
	}
	return nil
}

func nativeOnlyContext(plan *launch.Plan) error {
	if plan == nil || plan.ProviderBrand != "codex" || plan.ResumeSourceSessionID == "" || plan.ResumeProviderSessionID == "" {
		return nativeOnlyError("recorded Codex context required")
	}
	for _, path := range []string{plan.EffectiveWorkRoot(), plan.NativeStateRoot, filepath.Join(plan.NativeStateRoot, "sessions")} {
		if err := nativeOnlyDirectory(path); err != nil {
			return err
		}
	}
	directory, err := os.Open(filepath.Join(plan.NativeStateRoot, "sessions"))
	if err != nil {
		return nativeOnlyError("native state unavailable")
	}
	defer directory.Close()
	entries, err := directory.ReadDir(1)
	if err != nil || len(entries) == 0 {
		return nativeOnlyError("native state unavailable")
	}
	return nil
}

func (s *Service) validateNativeOnlySource(ctx context.Context, source *store.SessionRow, plan *launch.Plan) error {
	if source == nil || source.ID != plan.ResumeSourceSessionID || source.ProviderID != plan.ProviderID {
		return nativeOnlyError("source provider changed")
	}
	prior, err := s.Store.GetLaunchPlan(source.ID)
	if err != nil || prior.ProviderBrand != plan.ProviderBrand || prior.ProviderID != plan.ProviderID || prior.TeamMember || prior.NativeStateRoot != plan.NativeStateRoot || prior.EffectiveWorkRoot() != plan.EffectiveWorkRoot() {
		return nativeOnlyError("recorded source context changed")
	}
	mapping, err := s.Store.GetSessionProviderMapping(source.ID, "tether", plan.ProviderID)
	if err != nil || !mapping.NativeSessionID.Valid || mapping.NativeSessionID.String == "" || mapping.NativeSessionID.String != plan.ResumeProviderSessionID {
		return nativeOnlyError("recorded native mapping unavailable")
	}
	if _, err := s.Store.SessionShim(ctx, source.ID); !errors.Is(err, store.ErrSessionShimNotFound) {
		return nativeOnlyError("direct source custody unavailable")
	}
	if err := nativeOnlyContext(plan); err != nil {
		return err
	}
	home, err := captureCodexHome()
	if err != nil {
		return nativeOnlyError("approved credential source unavailable")
	}
	defer home.Close()
	if err := home.ValidateNativeLink(plan.NativeStateRoot); err != nil {
		return nativeOnlyError("native credential mapping unavailable")
	}
	return nil
}

func (r *recoveryRuntime) startNativeOnly(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	if !r.Caps().JsonRpcStdio || opts.SessionIDPreset == "" || opts.SessionIDPreset != r.plan.ResumeProviderSessionID {
		return nil, nativeOnlyError("existing-thread RPC context required")
	}
	if err := nativeOnlyContext(r.plan); err != nil {
		return nil, err
	}
	source, err := r.service.Store.GetSession(r.plan.ResumeSourceSessionID)
	if err != nil {
		return nil, nativeOnlyError("source unavailable")
	}
	if err := r.service.validateNativeOnlySource(ctx, source, r.plan); err != nil {
		return nil, err
	}
	home := ""
	for _, entry := range opts.Env {
		if value, ok := strings.CutPrefix(entry, "CODEX_HOME="); ok {
			if home != "" {
				return nil, nativeOnlyError("native home ambiguous")
			}
			home = value
		}
	}
	if opts.Workdir != r.plan.EffectiveWorkRoot() || home != r.plan.NativeStateRoot || opts.PreparedExecution != nil {
		return nil, nativeOnlyError("runtime context differs from recorded context")
	}
	credentialHome, err := captureCodexHome()
	if err != nil {
		return nil, nativeOnlyError("approved credential source unavailable")
	}
	defer credentialHome.Close()
	if err := credentialHome.ValidateNativeLink(r.plan.NativeStateRoot); err != nil {
		return nil, nativeOnlyError("native credential mapping unavailable")
	}
	roots := []string{r.plan.NativeStateRoot, r.plan.EffectiveWorkRoot(), filepath.Join(r.plan.NativeStateRoot, "sessions")}
	identities := make([]os.FileInfo, len(roots))
	for i, path := range roots {
		identities[i], err = os.Lstat(path)
		if err != nil {
			return nil, nativeOnlyError("recorded context unavailable")
		}
	}
	opts.AutoFireFirstTurn, opts.FirstTurnPayload = false, nil
	opts.BootPrompt, opts.BootContent, opts.BootMode = "", "", "none"
	onSessionID := opts.OnSessionID
	opts.OnSessionID = nil
	sess, err := r.Runtime.Start(ctx, opts)
	if err != nil {
		return nil, nativeOnlyError("native start refused")
	}
	done := make(chan error, 1)
	go func() { _, err := sess.Wait(); done <- err }()
	refuse := func(reason string) (agentsessions.Session, error) {
		if err := stopRecoveryAttempt(sess, done); err != nil {
			return nil, &recoveryUnconfirmedError{err: errors.Join(nativeOnlyError(reason), err), pid: sess.Health().PID}
		}
		return nil, nativeOnlyError(reason)
	}
	rpc, ok := sess.(agentsessions.JsonRpcCaller)
	if !ok {
		return refuse("native RPC unavailable")
	}
	handshake, cancel := context.WithTimeout(r.request, 5*time.Second)
	defer cancel()
	if _, err := rpc.Call(handshake, "initialize", map[string]any{"clientInfo": map[string]string{"name": "tether", "version": tetherClientVersion}}); err != nil {
		return refuse("native initialize refused")
	}
	response, err := rpc.Call(handshake, "thread/resume", map[string]any{"threadId": opts.SessionIDPreset, "excludeTurns": true, "cwd": opts.Workdir})
	if err != nil {
		return refuse("native thread resume refused")
	}
	actual, err := turn.DecodeCodexThreadID(response)
	if err != nil || actual != opts.SessionIDPreset {
		return refuse("native thread identity unconfirmed")
	}
	grace := r.service.resumeGrace
	if grace <= 0 {
		grace = nativeResumeGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-r.request.Done():
		return refuse("native resume canceled")
	case <-done:
		return nil, nativeOnlyError("native process ended before admission")
	case <-timer.C:
	}
	if !sess.Health().Alive {
		return refuse("native process health unconfirmed")
	}
	for i, path := range roots {
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(identities[i], current) {
			return refuse("recorded context identity changed")
		}
	}
	if err := credentialHome.ValidateNativeLink(r.plan.NativeStateRoot); err != nil {
		return refuse("native credential mapping changed")
	}
	if err := r.service.validateNativeOnlySource(r.request, source, r.plan); err != nil {
		return refuse("source context changed before admission")
	}
	if err := r.service.Store.UpsertSessionProviderMapping(r.id, "tether", r.plan.ProviderID, actual); err != nil {
		return refuse("native mapping persistence refused")
	}
	if onSessionID != nil {
		onSessionID(actual)
	}
	return sess, nil
}
