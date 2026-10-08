package oslog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Target kinds.
const (
	KindSimulator = "simulator"
	KindDevice    = "device"
)

// Target is a log source the user can stream from: a booted iOS simulator or a
// connected physical iOS device.
type Target struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Runtime   string `json:"runtime,omitempty"`
	Model     string `json:"model,omitempty"`
	Platform  string `json:"platform,omitempty"`
	OSVersion string `json:"osVersion,omitempty"`
	// Ready reports whether the target can be streamed from right now.
	Ready bool `json:"ready"`
	// Hint carries a actionable message when a target is not ready.
	Hint string `json:"hint,omitempty"`
}

// ListTargets discovers available simulators and connected devices. Simulators
// are always listed; devices require Xcode's devicectl and appear only when it
// is available.
func ListTargets(ctx context.Context) (simulators []Target, devices []Target, errs map[string]string) {
	errs = map[string]string{}

	sims, err := listSimulators(ctx)
	if err != nil {
		errs["simulators"] = err.Error()
	}
	devs, err := listDevices(ctx)
	if err != nil {
		errs["devices"] = err.Error()
	}
	return sims, devs, errs
}

type simctlDevices struct {
	Devices map[string][]struct {
		UDID                 string `json:"udid"`
		Name                 string `json:"name"`
		State                string `json:"state"`
		IsAvailable          bool   `json:"isAvailable"`
		DeviceTypeIdentifier string `json:"deviceTypeIdentifier"`
	} `json:"devices"`
}

func listSimulators(ctx context.Context) ([]Target, error) {
	out, err := runCommand(ctx, "xcrun", "simctl", "list", "devices", "--json")
	if err != nil {
		return nil, err
	}
	var parsed simctlDevices
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parsing `simctl list devices` output: %w", err)
	}

	var targets []Target
	for runtimeID, devices := range parsed.Devices {
		runtime := prettyRuntime(runtimeID)
		for _, d := range devices {
			if !d.IsAvailable {
				continue
			}
			t := Target{
				Kind:    KindSimulator,
				ID:      d.UDID,
				Name:    d.Name,
				State:   d.State,
				Runtime: runtime,
				Model:   strings.TrimPrefix(d.DeviceTypeIdentifier, "com.apple.CoreSimulator.SimDeviceType."),
				Ready:   d.State == "Booted",
			}
			if !t.Ready {
				t.Hint = "simulator is not booted"
			}
			targets = append(targets, t)
		}
	}

	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].Ready != targets[j].Ready {
			return targets[i].Ready
		}
		if targets[i].Runtime != targets[j].Runtime {
			return targets[i].Runtime > targets[j].Runtime
		}
		return targets[i].Name < targets[j].Name
	})
	return targets, nil
}

type devicectlDevices struct {
	Result struct {
		Devices []struct {
			Identifier       string `json:"identifier"`
			DeviceProperties struct {
				Name            string `json:"name"`
				OSVersionNumber string `json:"osVersionNumber"`
				MarketingName   string `json:"marketingName"`
				DeviceType      string `json:"deviceType"`
			} `json:"deviceProperties"`
			HardwareProperties struct {
				Platform   string `json:"platform"`
				DeviceType string `json:"deviceType"`
				UDID       string `json:"udid"`
			} `json:"hardwareProperties"`
			ConnectionProperties struct {
				TunnelState  string `json:"tunnelState"`
				PairingState string `json:"pairingState"`
			} `json:"connectionProperties"`
		} `json:"devices"`
	} `json:"result"`
}

func listDevices(ctx context.Context) ([]Target, error) {
	tmp, err := os.CreateTemp("", "devicectl-*.json")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	if _, err := runCommand(ctx, "xcrun", "devicectl", "list", "devices", "--json-output", path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var parsed devicectlDevices
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parsing `devicectl list devices` output: %w", err)
	}

	var targets []Target
	for _, d := range parsed.Result.Devices {
		platform := d.HardwareProperties.Platform
		if !strings.EqualFold(platform, "iOS") {
			continue
		}
		connected := strings.EqualFold(d.ConnectionProperties.TunnelState, "connected")
		paired := strings.EqualFold(d.ConnectionProperties.PairingState, "paired")
		t := Target{
			Kind:      KindDevice,
			ID:        d.Identifier,
			Name:      d.DeviceProperties.Name,
			State:     d.ConnectionProperties.TunnelState,
			Model:     d.DeviceProperties.DeviceType,
			Platform:  platform,
			OSVersion: d.DeviceProperties.OSVersionNumber,
			Ready:     connected && paired,
		}
		if !t.Ready {
			t.Hint = "device is not connected and paired"
		} else if os.Geteuid() != 0 {
			t.Hint = "streaming from a physical device requires the server to run with sudo"
		}
		targets = append(targets, t)
	}

	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].Ready != targets[j].Ready {
			return targets[i].Ready
		}
		return targets[i].Name < targets[j].Name
	})
	return targets, nil
}

// BootSimulator boots a shutdown simulator and brings up the Simulator app so it
// becomes streamable.
func BootSimulator(ctx context.Context, udid string) error {
	if udid == "" {
		return fmt.Errorf("simulator udid is required")
	}
	if _, err := runCommand(ctx, "xcrun", "simctl", "boot", udid); err != nil {
		return err
	}
	// Best effort: failing to open the UI must not fail the boot.
	_, _ = runCommand(ctx, "open", "-a", "Simulator")
	return nil
}

// prettyRuntime turns a simctl runtime identifier such as
// "com.apple.CoreSimulator.SimRuntime.iOS-26-5" into "iOS 26.5".
func prettyRuntime(runtimeID string) string {
	s := runtimeID
	if idx := strings.LastIndex(s, "SimRuntime."); idx >= 0 {
		s = s[idx+len("SimRuntime."):]
	}
	parts := strings.Split(s, "-")
	if len(parts) < 2 {
		return s
	}
	return parts[0] + " " + strings.Join(parts[1:], ".")
}

// runCommand executes a command and returns stdout. Stderr is folded into the
// error so callers can surface actionable messages (for example the "Must be
// admin" notice from `log stream`).
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), firstLine(msg))
	}
	return out, nil
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
