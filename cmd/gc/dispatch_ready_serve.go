package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Installed only during a recognized control query's --follow lifetime.
// One-shot callers and custom queries retain their existing store paths.
var (
	controlReadyServeScopeRead     func(dir, cityPath string, env map[string]string, includeEphemeral bool) ([]beads.Bead, bool, error)
	controlReadyServeReaderFactory = newControlReadyServeReaders
)

var errControlReadyPreflightFallback = errors.New("readiness preflight requires retryable CLI fallback")

func withControlReadyServeReaders(query string, run func() error) (err error) {
	if _, recognized := parseControlReadyQuery(query); !recognized {
		return run()
	}
	readers, read := controlReadyServeReaderFactory()
	previous := controlReadyServeScopeRead
	controlReadyServeScopeRead = read
	defer func() { controlReadyServeScopeRead = previous; err = errors.Join(err, readers.close()) }()
	return run()
}

func newControlReadyServeReaders() (*controlReadyReaders, func(string, string, map[string]string, bool) ([]beads.Bead, bool, error)) {
	readers := &controlReadyReaders{}
	var mu sync.Mutex
	read := func(dir, cityPath string, env map[string]string, include bool) ([]beads.Bead, bool, error) {
		// Bind each opening closure to this call's exact snapshot, including when
		// an input changes between capture and the SDK open. No shared mutable
		// expected fingerprint can race another caller.
		mu.Lock()
		defer mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		capture := func() (*controlReadyOpeningSnapshot, error) {
			return captureControlReadyServeOpening(ctx, dir, cityPath, env)
		}
		provider := rawBeadsProviderForScope(dir, cityPath)
		if !contract.ProviderUsesBDContract(provider) {
			return nil, false, nil
		}
		before, err := capture()
		if err != nil {
			return nil, true, err
		}
		readers.open = func(ctx context.Context, root string, selected map[string]string) (*controlReadyReader, bool, error) {
			if before.FallbackReason != "" || beads.NativeReadyFallbackReason(root, provider, selected) != "" {
				return nil, false, nil
			}
			return controlReadyOpenVerified(ctx, before, func(ctx context.Context, opening map[string]string) (*controlReadyReader, bool, error) {
				verdict, err := newBeadsPreflightChecker(cityPath, provider).Check(root)
				if err != nil {
					return nil, false, errors.Join(errControlReadyPreflightFallback, err)
				}
				if !verdict.NativeStoreEligible {
					return nil, false, fmt.Errorf("%w: %s", errControlReadyPreflightFallback, verdict.FallbackReason)
				}
				native, err := beads.OpenNativeReadyReader(ctx, opening["BEADS_DIR"], opening)
				if err != nil {
					return nil, false, err
				}
				return controlReadyNativeScopeReader(native.Ready, native.Close), true, nil
			}, capture)
		}
		rows, handled, err := readers.ready(ctx, dir, before.Fingerprint, before.Env, include)
		if errors.Is(err, errControlReadyPreflightFallback) {
			return nil, false, nil
		}
		if err == nil && len(rows) == controlReadyFallbackLimit {
			log.Printf("control-ready native frontier for %s reached the %d-item limit", dir, controlReadyFallbackLimit)
		}
		return rows, handled, err
	}
	return readers, read
}

func captureControlReadyServeOpening(ctx context.Context, dir, cityPath string, env map[string]string) (*controlReadyOpeningSnapshot, error) {
	ambient := beads.ProcessEnvSnapshotExcludingNativeDoltOpen()
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return nil, err
	}
	var canonical map[string]string
	if samePath(dir, cityPath) {
		canonical, err = bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, false)
	} else {
		canonical, err = bdRuntimeEnvForRigWithErrorRecoveryContext(ctx, cityPath, cfg, dir, false)
	}
	if err != nil {
		return nil, err
	}
	// Canonical projection fills defaults. Divergent effective CLI selections
	// (explicit or ambient) remain on the existing CLI path, never retargeted.
	selected := make(map[string]string, len(env)+len(canonical))
	for _, item := range mergeRuntimeEnv(ambient, env) {
		if key, value, ok := strings.Cut(item, "="); ok {
			selected[key] = value
		}
	}
	explicitAuthority := false
	for key, value := range canonical {
		if original, exists := selected[key]; exists && strings.HasPrefix(key, "BEADS_") && original != value {
			// Preserve a deliberate CLI ledger/endpoint selection. When the
			// invocation's old projection diverges, the existing CLI path is
			// also safer than silently retargeting it to another database.
			explicitAuthority = true
			continue
		}
		selected[key] = value
	}
	provider := rawBeadsProviderForScope(dir, cityPath)
	configuration, err := json.Marshal(struct {
		City     *config.City
		Provider string
	}{cfg, provider})
	if err != nil {
		return nil, err
	}
	snapshot, err := controlReadyCaptureOpening(fsys.OSFS{}, dir, mergeRuntimeEnv(ambient, selected), configuration)
	if err != nil {
		return nil, err
	}
	// The adapter only projects BEADS_*; a custom subprocess home must stay
	// with the CLI rather than silently use the SDK process's home defaults.
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		if snapshot.Env[key] != envListValue(ambient, key) {
			snapshot.FallbackReason = "custom CLI home configuration"
			break
		}
	}
	if explicitAuthority {
		snapshot.FallbackReason = "explicit CLI ledger or endpoint differs from canonical projection"
	}
	snapshot.Fingerprint = controlReadyOpeningFingerprint(snapshot.Env, []byte(snapshot.Fingerprint+"\x00"+snapshot.FallbackReason), nil)
	return snapshot, nil
}
