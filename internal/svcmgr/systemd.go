package svcmgr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// systemdUser drives a `systemctl --user`-managed mtroamd. Its
// Stop/Start/Restart all set XDG_RUNTIME_DIR + DBUS_SESSION_BUS_ADDRESS
// explicitly so they work when invoked from a non-pam_systemd SSH
// session (the common case for our installer-spawned shells).
type systemdUser struct{}

const systemdUnitName = "mtroamd"

func (s *systemdUser) Name() string { return "systemd-user" }

func (s *systemdUser) Available(ctx context.Context) bool {
	if !commandExists("systemctl") {
		return false
	}
	rd := userRuntimeDir()
	if rd == "" {
		return false
	}
	// Reachability signal = the systemd USER MANAGER's private control
	// socket, NOT $XDG_RUNTIME_DIR/bus. `systemctl --user` talks to the
	// manager over `.../systemd/private`; `/bus` is the D-Bus *session*
	// bus, which a headless / SSH / lingering-headless login never
	// creates. Gating on `/bus` reported a genuinely systemd-managed
	// daemon (linger on, `systemctl --user is-active mtroamd` = active,
	// but no `/bus`) as unmanaged → Detect fell back to nohup, so
	// restart/update drove the wrong backend and doctor's linger/unit
	// checks (gated on Name()=="systemd-user") were suppressed. The
	// private socket exists iff the per-user manager is running — exactly
	// the condition `systemctl --user` needs. Stat rather than try-and-
	// fail so Detect can fall back to nohup cleanly.
	if _, err := os.Stat(managerControlSocket(rd)); err != nil {
		return false
	}
	// Last check: our unit must be installed in one of systemd's --user
	// unit dirs. Look in ALL of them, not just ~/.config: the .deb/.rpm
	// drop the unit in /usr/lib/systemd/user, so a hardcoded ~/.config
	// check falsely reports a package install as unmanaged → Detect falls
	// back to nohup even though `systemctl --user` (which searches every
	// dir) runs it fine.
	return s.installedUnitPath() != ""
}

func (s *systemdUser) Stop(ctx context.Context) error {
	if !s.Available(ctx) {
		return ErrUnavailable
	}
	// `stop` on a not-running unit returns 0 from recent systemd;
	// older versions return 5. Either way we treat any non-fatal
	// exit as "stopped".
	cmd := s.cmd(ctx, "stop", systemdUnitName)
	_ = cmd.Run()
	return nil
}

func (s *systemdUser) Start(ctx context.Context, binPath string) error {
	if !s.Available(ctx) {
		return ErrUnavailable
	}
	cmd := s.cmd(ctx, "start", systemdUnitName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user start: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *systemdUser) Restart(ctx context.Context, binPath string) error {
	if !s.Available(ctx) {
		return ErrUnavailable
	}
	// Use a single restart rather than stop-then-start; systemd
	// handles the inter-process race (port re-bind on the same
	// addr) better than we can manually.
	cmd := s.cmd(ctx, "restart", systemdUnitName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user restart: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *systemdUser) Remove(ctx context.Context) error {
	if !s.Available(ctx) {
		// Try the no-bus path: remove the unit file directly so a
		// future login that brings the user manager up won't see
		// our ghost unit.
		return os.Remove(s.unitPath())
	}
	// disable --now stops + un-enables; we follow with a daemon-reload
	// so the unit's gone from systemctl's in-memory index.
	_ = s.cmd(ctx, "disable", "--now", systemdUnitName).Run()
	if err := os.Remove(s.unitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit file: %w", err)
	}
	_ = s.cmd(ctx, "daemon-reload").Run()
	return nil
}

// cmd constructs an exec.Cmd invoking `systemctl --user <args>` with
// the env vars that make the user-bus reachable from a non-pam_systemd
// session.
func (s *systemdUser) cmd(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{"--user"}, args...)
	cmd := exec.CommandContext(ctx, "systemctl", full...)
	cmd.Env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+userRuntimeDir(),
		"DBUS_SESSION_BUS_ADDRESS=unix:path="+userRuntimeDir()+"/bus",
	)
	return cmd
}

// unitPath is the canonical, WRITABLE per-user unit location — the target
// `mtroamd migrate` reads/rewrites and where the iOS installer drops the unit.
// (Not the package location, which is root-owned and read-only to the user.)
func (s *systemdUser) unitPath() string {
	return homePath(".config", "systemd", "user", "mtroamd.service")
}

// unitSearchPaths lists the systemd --user unit locations we install to or
// support, in systemd's own precedence order: the per-user dir (iOS
// auto-installer, `mtroamd migrate`, manual installs), the admin override
// dir, and the packaged vendor dir (the .deb/.rpm drop the unit here).
func (s *systemdUser) unitSearchPaths() []string {
	return []string{
		s.unitPath(),
		"/etc/systemd/user/mtroamd.service",
		"/usr/lib/systemd/user/mtroamd.service",
	}
}

// installedUnitPath returns the first unit-file location that exists, or ""
// if the unit isn't installed anywhere systemd would find it.
func (s *systemdUser) installedUnitPath() string {
	for _, p := range s.unitSearchPaths() {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// UnitPath exposes the unit-file location for the doctor command — the path
// where the unit is actually installed (so a package install reports
// /usr/lib/... rather than a non-existent ~/.config path), falling back to
// the canonical user location when nothing is installed yet.
func (s *systemdUser) UnitPath() string {
	if p := s.installedUnitPath(); p != "" {
		return p
	}
	return s.unitPath()
}

// SystemctlUser runs `systemctl --user <args>` with the env that makes the
// user bus reachable from a non-pam_systemd session (matching the env used
// by Stop/Start/Restart). Exposed for `mtroamd migrate`, which rewrites
// the unit then reloads/restarts/queries outside the Manager interface.
// Returns combined stdout+stderr for diagnostics.
func SystemctlUser(ctx context.Context, args ...string) ([]byte, error) {
	s := &systemdUser{}
	return s.cmd(ctx, args...).CombinedOutput()
}

// UserUnitPath returns the canonical systemd --user unit path
// (~/.config/systemd/user/mtroamd.service), independent of whether the
// manager is currently Available. Used by `mtroamd migrate` to read/rewrite
// the unit even when the bus is momentarily unreachable.
func UserUnitPath() string {
	return (&systemdUser{}).unitPath()
}

func userRuntimeDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return v
	}
	// pam_systemd's convention. Same value the iOS installer uses.
	return fmt.Sprintf("/run/user/%d", os.Getuid())
}

// managerControlSocket is the systemd per-user manager's private control
// socket under the runtime dir — the endpoint `systemctl --user` uses.
// Present iff `user@<uid>.service` is running. Factored out so Available's
// reachability gate is unit-testable against a temp runtime dir.
func managerControlSocket(runtimeDir string) string {
	return runtimeDir + "/systemd/private"
}
