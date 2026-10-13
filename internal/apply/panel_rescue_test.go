package apply

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crmne/hyprmoncfg/internal/hypr"
	"github.com/crmne/hyprmoncfg/internal/profile"
)

// Issue #67: on an i915 laptop with the lid closed, the reloaded config turns
// the rescue panel on but leaves it without a mode, and the external stays
// dark until the panel's rule is set live.
const (
	rescuePanelOff     = `{"id":1,"name":"eDP-1","description":"BOE Panel","make":"BOE","model":"Panel","serial":"I1","width":0,"height":0,"refreshRate":0,"x":0,"y":0,"scale":1,"transform":0,"dpmsStatus":true,"disabled":true,"mirrorOf":"","availableModes":["1920x1080@144.00Hz"]}`
	rescuePanelNoMode  = `{"id":1,"name":"eDP-1","description":"BOE Panel","make":"BOE","model":"Panel","serial":"I1","width":0,"height":0,"refreshRate":60,"x":2560,"y":0,"scale":1,"transform":0,"dpmsStatus":true,"disabled":false,"mirrorOf":"","availableModes":["1920x1080@144.00Hz"]}`
	rescuePanelLit     = `{"id":1,"name":"eDP-1","description":"BOE Panel","make":"BOE","model":"Panel","serial":"I1","width":1920,"height":1080,"refreshRate":144,"x":2560,"y":0,"scale":1,"transform":0,"dpmsStatus":true,"disabled":false,"mirrorOf":"","availableModes":["1920x1080@144.00Hz"]}`
	rescueHDMIModeless = `{"id":2,"name":"HDMI-A-1","description":"HKC 27E1QA","make":"HKC","model":"27E1QA","serial":"E1","width":0,"height":0,"refreshRate":0,"x":0,"y":0,"scale":1,"transform":0,"dpmsStatus":true,"disabled":false,"mirrorOf":"","availableModes":["2560x1440@144.00Hz"]}`
)

func rescueEngine(t *testing.T, liveRuleLights bool) (Engine, string, profile.Profile, []hypr.Monitor) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hyprctl.log")
	state := filepath.Join(dir, "state")
	lights := "0"
	if liveRuleLights {
		lights = "1"
	}
	script := `#!/usr/bin/env bash
set -eu
if [ "${1-}" = "--instance" ]; then shift 2; fi
printf '%s\n' "$*" >> "$HYPRCTL_LOG"
stage="$(cat "$RESCUE_STATE" 2>/dev/null || echo before)"
case "$1 ${2-} ${3-}" in
  "-j version "*) printf '{"version":"0.54.0"}' ;;
  "-j monitors all")
    case "$stage" in
      before) printf '%s' '[` + rescuePanelOff + `,` + rescueHDMIModeless + `]' ;;
      reloaded) printf '%s' '[` + rescuePanelNoMode + `,` + rescueHDMIModeless + `]' ;;
      lit) printf '%s' '[` + rescuePanelLit + `,` + rescueHDMIModeless + `]' ;;
    esac ;;
  "-j workspacerules "*|"-j workspaces "*) printf '[]' ;;
  "reload "*) echo reloaded > "$RESCUE_STATE" ;;
  "keyword monitor "*)
    if [ "$RESCUE_LIGHTS" = 1 ]; then echo lit > "$RESCUE_STATE"; fi
    printf 'ok' ;;
  "--batch "*) printf 'ok' ;;
  *) echo "unexpected args: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "hyprctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	hyprland := filepath.Join(dir, "hyprland.conf")
	if err := os.WriteFile(hyprland, []byte("# root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HYPRCTL_LOG", logPath)
	t.Setenv("RESCUE_STATE", state)
	t.Setenv("RESCUE_LIGHTS", lights)
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "sig-test")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	client, err := hypr.NewClient()
	if err != nil {
		t.Fatal(err)
	}

	panel := hypr.Monitor{Name: "eDP-1", Description: "BOE Panel", Make: "BOE", Model: "Panel", Serial: "I1", Disabled: true,
		AvailableModes: []string{"1920x1080@144.00Hz"}}
	hdmi := hypr.Monitor{Name: "HDMI-A-1", Description: "HKC 27E1QA", Make: "HKC", Model: "27E1QA", Serial: "E1", DPMSStatus: true,
		AvailableModes: []string{"2560x1440@144.00Hz"}}
	monitors := []hypr.Monitor{panel, hdmi}
	saved := profile.New("casa", []profile.OutputConfig{
		{Key: panel.HardwareKey(), Name: panel.Name, Description: panel.Description, Enabled: false, Width: 1920, Height: 1080, Refresh: 144, Scale: 1},
		{Key: hdmi.HardwareKey(), Name: hdmi.Name, Description: hdmi.Description, Enabled: true, Width: 2560, Height: 1440, Refresh: 144, Scale: 1},
	})
	rescued, lit := profile.KeepBuiltInPanelOn(saved, monitors)
	if len(lit) != 1 {
		t.Fatalf("expected the panel to be kept on, got %v", lit)
	}
	engine := Engine{Client: client, MonitorsConfPath: filepath.Join(dir, "hyprmoncfg-monitors.conf"),
		HyprlandConfigPath: hyprland, TolerateModeless: true}
	return engine, logPath, rescued, monitors
}

func TestEngineApplySetsTheRescuePanelModeLiveWhenTheReloadLeavesItModeless(t *testing.T) {
	engine, logPath, rescued, monitors := rescueEngine(t, true)
	var logged []string
	engine.Logf = func(format string, args ...any) { logged = append(logged, format) }
	if _, err := engine.Apply(context.Background(), rescued, monitors); err != nil {
		t.Fatalf("setting the panel's mode live should complete the rescue: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "keyword monitor eDP-1,1920x1080@144.00,2560x0,1") {
		t.Fatalf("the panel's rule should be set live, got:\n%s", log)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "came on without a mode") {
		t.Fatalf("the live modeset should be logged, got %v", logged)
	}
}

func TestEngineApplyFailsWhileTheRescuePanelHasNoMode(t *testing.T) {
	restore := applyValidationTimeout
	applyValidationTimeout = time.Second
	defer func() { applyValidationTimeout = restore }()
	engine, logPath, rescued, monitors := rescueEngine(t, false)
	_, err := engine.Apply(context.Background(), rescued, monitors)
	// The error is the mode mismatch, or the query cut short by the deadline.
	if err == nil {
		t.Fatalf("a rescue panel without a mode lights nothing; the apply must fail so it is retried, got %v", err)
	}
	log, _ := os.ReadFile(logPath)
	if n := strings.Count(string(log), "keyword monitor eDP-1,1920x1080"); n != 1 {
		t.Fatalf("the panel's rule should be set live once per apply, got %d:\n%s", n, log)
	}
}
