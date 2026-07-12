package svcmgr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSystemdUserAvailable pins the reachability gate that the 2026-07-12
// fix corrected: Available must key on the systemd user-manager's PRIVATE
// control socket ($XDG_RUNTIME_DIR/systemd/private) — what `systemctl
// --user` actually uses — NOT the D-Bus session bus ($XDG_RUNTIME_DIR/bus),
// which a headless/SSH/lingering login never creates. The old /bus gate
// misreported a genuinely systemd-managed daemon (linger on, is-active =
// active) as nohup.
func TestSystemdUserAvailable(t *testing.T) {
	if !commandExists("systemctl") {
		t.Skip("systemctl not on PATH")
	}

	runtimeDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("HOME", home)

	// Install a user unit so installedUnitPath() is satisfied.
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "mtroamd.service"), []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &systemdUser{}
	ctx := context.Background()

	// No manager private socket yet → not available (manager not running).
	if s.Available(ctx) {
		t.Fatal("Available should be false with no systemd/private socket")
	}

	// The D-Bus session bus must NOT satisfy the gate — this was the bug.
	if err := os.WriteFile(filepath.Join(runtimeDir, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Available(ctx) {
		t.Fatal("Available must not be satisfied by the D-Bus /bus socket (regression guard)")
	}

	// The manager's private control socket present → available.
	if err := os.MkdirAll(filepath.Join(runtimeDir, "systemd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managerControlSocket(runtimeDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.Available(ctx) {
		t.Fatal("Available should be true once the systemd/private socket exists")
	}

	// Removing the per-user unit makes Available false — UNLESS a
	// system-wide unit (/etc or /usr/lib/systemd/user, absolute paths
	// t.Setenv can't sandbox) shadows it, in which case Available
	// correctly stays true. Assert the right outcome for THIS host either
	// way, so coverage holds on both packaged and unpackaged boxes.
	if err := os.Remove(filepath.Join(unitDir, "mtroamd.service")); err != nil {
		t.Fatal(err)
	}
	systemUnit := fileExists("/etc/systemd/user/mtroamd.service") ||
		fileExists("/usr/lib/systemd/user/mtroamd.service")
	if got := s.Available(ctx); got != systemUnit {
		t.Fatalf("Available after removing the user unit = %v, want %v (system unit present = %v)",
			got, systemUnit, systemUnit)
	}
}
