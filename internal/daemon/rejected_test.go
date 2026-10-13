package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crmne/hyprmoncfg/internal/apply"
	"github.com/crmne/hyprmoncfg/internal/hypr"
	"github.com/crmne/hyprmoncfg/internal/ipc"
	"github.com/crmne/hyprmoncfg/internal/profile"
)

func TestFirstSetupPreservesLiveModeRotationAndPlacement(t *testing.T) {
	monitors := []hypr.Monitor{
		{Name: "HDMI-A-1", Make: "LG", Model: "TV", Width: 3840, Height: 2160, RefreshRate: 60, X: 1080, Scale: 1, DPMSStatus: true, VRR: 1, CurrentFormat: "XBGR8888", AvailableModes: []string{"4096x2160@119.88Hz", "3840x2160@60Hz"}},
		{Name: "DP-3", Make: "Dell", Model: "Portrait", Width: 1920, Height: 1080, RefreshRate: 60, Y: 107, Scale: 1, Transform: 1, DPMSStatus: true},
	}
	data, _ := json.Marshal(monitors)
	env := newApplyBestTestEnvWithMonitors(t, string(data), string(data))
	svc := New(env.client, env.store, Config{MonitorsConf: env.monitorsConfPath, HyprConfig: env.hyprlandConfigPath})
	if err := svc.applyBest(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := profile.FromMonitors("draft", monitors).Outputs
	for i := range want {
		// Live VRR readback is activity, not the configured mode.
		want[i].VRR, want[i].Bitdepth = 0, 8
	}
	if !reflect.DeepEqual(svc.applied.requested.Outputs, want) {
		t.Fatalf("startup changed working displays: %+v", svc.applied.requested.Outputs)
	}
	before := reloadCount(env.logPath)
	for range 3 {
		if err := svc.applyBest(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if reloadCount(env.logPath) != before {
		t.Fatal("unchanged unsaved setup kept applying")
	}
	// A fullscreen client toggling VRR activity is not a new request.
	monitors[0].VRR = 0
	data, _ = json.Marshal(monitors)
	override := filepath.Join(t.TempDir(), "monitors.json")
	if err := os.WriteFile(override, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HYPRCTL_MONITORS_OVERRIDE", override)
	if err := svc.applyBest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reloadCount(env.logPath) != before {
		t.Fatal("VRR activity change reapplied the unsaved setup")
	}
}

func TestLiveDraftStillEnablesModelessNewDisplay(t *testing.T) {
	working := hypr.Monitor{Name: "DP-1", Width: 1920, Height: 1080, RefreshRate: 60, Scale: 1, Transform: 1, DPMSStatus: true}
	newDisplay := hypr.Monitor{Name: "HDMI-A-1", Disabled: true, AvailableModes: []string{"1920x1080@60Hz"}}
	p := liveDraft([]hypr.Monitor{working, newDisplay}, nil)
	out, _ := p.OutputByKey(working.HardwareKey())
	if out.Transform != 1 || out.Width != 1920 {
		t.Fatalf("working display changed: %+v", out)
	}
	added, _ := p.OutputByKey(newDisplay.HardwareKey())
	if !added.Enabled || added.Width != 1920 || added.X != 1080 {
		t.Fatalf("new display was not extended: %+v", added)
	}
	if want := []string{working.HardwareKey(), newDisplay.HardwareKey()}; !reflect.DeepEqual(p.Workspaces.MonitorOrder, want) {
		t.Fatalf("workspace order = %v, want %v", p.Workspaces.MonitorOrder, want)
	}
}

// Hyprland's synthetic head must never be laid out, enabled or given
// workspaces by an unsaved setup, even if it reaches the draft unfiltered.
func TestLiveDraftExcludesSyntheticFallbackHead(t *testing.T) {
	working := hypr.Monitor{Name: "DP-1", Make: "Dell", Model: "U2720Q", Width: 2560, Height: 1440, RefreshRate: 60, Scale: 1, DPMSStatus: true}
	synthetic := hypr.Monitor{Name: "FALLBACK", Width: 1920, Height: 1080, RefreshRate: 60, X: 2560, Scale: 1, DPMSStatus: true, AvailableModes: []string{"1920x1080@60Hz"}}
	modeless := synthetic
	modeless.Disabled, modeless.Width, modeless.Height = true, 0, 0
	rules := []hypr.WorkspaceRule{
		{WorkspaceString: "1", Monitor: "DP-1"},
		{WorkspaceString: "2", Monitor: "FALLBACK"},
	}
	for _, head := range []hypr.Monitor{synthetic, modeless} {
		p := liveDraft([]hypr.Monitor{working, head}, rules)
		if len(p.Outputs) != 1 || p.Outputs[0].Name != "DP-1" {
			t.Fatalf("draft includes the synthetic head: %+v", p.Outputs)
		}
		for _, key := range p.Workspaces.MonitorOrder {
			if key != p.Outputs[0].Key {
				t.Fatalf("workspace order includes the synthetic head: %v", p.Workspaces.MonitorOrder)
			}
		}
		for _, rule := range p.Workspaces.Rules {
			if rule.OutputName == "FALLBACK" || rule.Workspace == "2" {
				t.Fatalf("workspace rule targets the synthetic head: %+v", p.Workspaces.Rules)
			}
		}
	}
}

// The daemon rebuilds an unsaved draft from live state on every pass, so
// telemetry must not change the request and lift a rejection pause.
func TestRejectedLiveDraftIgnoresTelemetry(t *testing.T) {
	monitors := []hypr.Monitor{{Name: "DP-1", Make: "Dell", Model: "U2720Q", Width: 2560, Height: 1440, RefreshRate: 60, Scale: 1, DPMSStatus: true, VRR: 1, CurrentFormat: "XRGB8888"}}
	r := rememberRejected(liveDraft(monitors, nil), monitors, nil)
	monitors[0].VRR = 0
	monitors[0].CurrentFormat = "XRGB2101010"
	if !r.matches(liveDraft(monitors, nil), monitors, nil) {
		t.Fatal("VRR activity or buffer format lifted the rejection pause")
	}
	monitors[0].Transform = 1
	if r.matches(liveDraft(monitors, nil), monitors, nil) {
		t.Fatal("a real layout change did not lift the rejection pause")
	}
}

func TestRejectedAutomaticLayoutStopsRetriesAndAcceptsEditedProfile(t *testing.T) {
	monitors := []hypr.Monitor{{Name: "HDMI-A-1", Make: "LG", Model: "TV", Width: 3840, Height: 2160, RefreshRate: 60, Scale: 1, DPMSStatus: true}}
	env := newRunTestEnv(t, monitors, func(s *Service) {
		s.cfg.RecoveryInterval = 10 * time.Millisecond
		s.cfg.RecoveryMaxInterval = 20 * time.Millisecond
		s.cfg.PollInterval = 10 * time.Millisecond
		p := profile.FromMonitors("Home", monitors)
		p.Outputs[0].Width, p.Outputs[0].Mode = 4096, "4096x2160@119.88Hz"
		if err := s.store.Save(p); err != nil {
			t.Fatal(err)
		}
	})
	defer env.stop()
	waitFor(t, 6*time.Second, func() bool { return env.logs.contains("automatic retry paused:") }, "rejected mode guard")
	count := reloadCount(env.logPath)
	if count != 2 {
		t.Fatalf("wanted one apply and one rollback, got %d", count)
	}
	time.Sleep(150 * time.Millisecond)
	if reloadCount(env.logPath) != count {
		t.Fatalf("rejected configuration kept reloading: %s", env.logs.all())
	}
	if err := env.svc.applyAutomatic(context.Background()); !errors.Is(err, errAutomaticApplyRejected) {
		t.Fatalf("repeated event was not blocked: %v", err)
	}
	if reloadCount(env.logPath) != count {
		t.Fatal("explicit reconciliation repeated rejected apply")
	}
	// Turning automatic selection on asks for another try, which this
	// display rejects again.
	_ = env.svc.SetProfileAuto(ipc.ProfileAutoParams{Enabled: true})
	if reloadCount(env.logPath) <= count {
		t.Fatal("turning automatic selection on did not retry the layout")
	}
	count = reloadCount(env.logPath)
	if err := env.svc.store.Save(profile.FromMonitors("Home", monitors)); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.applyAutomatic(context.Background()); err != nil {
		t.Fatalf("corrected profile blocked: %v", err)
	}
	if reloadCount(env.logPath) != count+1 {
		t.Fatal("corrected profile was not applied")
	}
}

func TestRejectedGuardDoesNotBlockModelessRecoveryOrChangedState(t *testing.T) {
	monitors := []hypr.Monitor{{Name: "DP-1", Width: 1920, Height: 1080, RefreshRate: 60, Scale: 1, DPMSStatus: true}}
	p := profile.FromMonitors("Desk", monitors)
	r := rememberRejected(p, monitors, nil)
	if !r.matches(p, monitors, nil) {
		t.Fatal("guard did not match")
	}
	p.CreatedAt, p.UpdatedAt = time.Now(), time.Now()
	if !r.matches(p, monitors, nil) {
		t.Fatal("regenerated draft timestamp restarted rejected apply")
	}
	monitors[0].VRR = 1
	monitors[0].CurrentFormat = "XBGR2101010"
	if !r.matches(p, monitors, nil) {
		t.Fatal("live VRR or buffer format restarted rejected apply")
	}
	monitors[0].Width = 0
	if r.matches(p, monitors, nil) {
		t.Fatal("guard blocked modeless recovery")
	}
	monitors[0].Width = 1280
	if r.matches(p, monitors, nil) {
		t.Fatal("guard blocked changed live state")
	}
	monitors[0].Width = 1920
	p.Outputs[0].Refresh = 30
	if r.matches(p, monitors, nil) {
		t.Fatal("guard blocked changed request")
	}
}

func TestRejectedApplyRetainsVerificationError(t *testing.T) {
	monitors := []hypr.Monitor{{Name: "DP-1", Width: 1920, Height: 1080, RefreshRate: 60, Scale: 1, DPMSStatus: true}}
	data, _ := json.Marshal(monitors)
	env := newApplyBestTestEnvWithMonitors(t, string(data), string(data))
	p := profile.FromMonitors("Desk", monitors)
	p.Outputs[0].Width, p.Outputs[0].Mode = 3840, "3840x1080@60Hz"
	if err := env.store.Save(p); err != nil {
		t.Fatal(err)
	}
	svc := New(env.client, env.store, Config{MonitorsConf: env.monitorsConfPath, HyprConfig: env.hyprlandConfigPath})
	err := svc.applyAutomatic(context.Background())
	if !errors.Is(err, apply.ErrVerificationFailed) || !errors.Is(err, errAutomaticApplyRejected) {
		t.Fatalf("lost failure cause: %v", err)
	}
	contents, _ := os.ReadFile(env.monitorsConfPath)
	if strings.Contains(string(contents), "3840x1080") {
		t.Fatal("rejected config was not rolled back")
	}
}

// A rollback that did not complete can leave the rejected config on disk for
// the next reload. Only a confirmed restore may pause automatic retries.
func TestRejectedApplyWithFailedRollbackKeepsRetrying(t *testing.T) {
	monitors := []hypr.Monitor{{Name: "DP-1", Width: 1920, Height: 1080, RefreshRate: 60, Scale: 1, DPMSStatus: true}}
	data, _ := json.Marshal(monitors)
	env := newApplyBestTestEnvWithMonitors(t, string(data), string(data))
	dir := filepath.Dir(env.logPath)
	if err := os.Rename(filepath.Join(dir, "hyprctl"), filepath.Join(dir, "hyprctl.real")); err != nil {
		t.Fatal(err)
	}
	// The apply's reload succeeds; the rollback's reload fails.
	wrapper := `#!/bin/bash
args=("$@")
if [[ "${1-}" == "--instance" ]]; then shift 2; fi
if [[ "${1-}" == "reload" && -f "$HYPRCTL_STATE" ]]; then
  printf '%s\n' "$*" >> "$HYPRCTL_LOG"
  echo "reload failed" >&2
  exit 1
fi
exec "$(dirname "$0")/hyprctl.real" "${args[@]}"
`
	if err := os.WriteFile(filepath.Join(dir, "hyprctl"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	p := profile.FromMonitors("Desk", monitors)
	p.Outputs[0].Width, p.Outputs[0].Mode = 3840, "3840x1080@60Hz"
	if err := env.store.Save(p); err != nil {
		t.Fatal(err)
	}
	svc := New(env.client, env.store, Config{MonitorsConf: env.monitorsConfPath, HyprConfig: env.hyprlandConfigPath})
	err := svc.applyAutomatic(context.Background())
	if !errors.Is(err, apply.ErrVerificationFailed) || !errors.Is(err, apply.ErrRollbackFailed) {
		t.Fatalf("lost failure cause: %v", err)
	}
	if errors.Is(err, errAutomaticApplyRejected) || svc.rejected != nil {
		t.Fatalf("failed rollback paused automatic retries: %v", err)
	}
	if reloadCount(env.logPath) != 2 {
		t.Fatalf("expected the apply reload and the failed rollback reload, got %d", reloadCount(env.logPath))
	}
}
