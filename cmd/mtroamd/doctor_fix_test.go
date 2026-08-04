package main

import (
	"reflect"
	"testing"
)

func kindsOf(fixes []plannedFix) []doctorFixKind {
	out := make([]doctorFixKind, 0, len(fixes))
	for _, f := range fixes {
		out = append(out, f.kind)
	}
	return out
}

// unit / linger states shared across cases.
var (
	unitOK       = &UnitFileInfo{Backend: "systemd-user", Present: true, KillModeProcess: true}
	unitMissing  = &UnitFileInfo{Backend: "systemd-user", Present: false}
	unitNoKill   = &UnitFileInfo{Backend: "systemd-user", Present: true, KillModeProcess: false}
	lingerOn     = &LingerInfo{User: "u", Enabled: true}
	lingerOff    = &LingerInfo{User: "u", Enabled: false}
	lingerErrOff = &LingerInfo{User: "u", Enabled: false, Error: "loginctl missing"}
)

func rep(running bool, ver string, procs []serveProcess, unit *UnitFileInfo, linger *LingerInfo) DoctorReport {
	return DoctorReport{
		Daemon:    DaemonHealth{Running: running, Version: ver},
		Processes: procs,
		UnitFile:  unit,
		Linger:    linger,
	}
}

func TestPlanFixes(t *testing.T) {
	const self = "v1.7.8"
	cases := []struct {
		name      string
		report    DoctorReport
		backend   string
		available bool
		goos      string
		selfVer   string
		want      []doctorFixKind
	}{
		{
			name:    "all healthy - nothing to fix",
			report:  rep(true, "v1.7.8 (abc, built …)", nil, unitOK, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: nil,
		},
		{
			name:    "daemon down + systemd → start",
			report:  rep(false, "", nil, unitOK, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{fixStartDaemon},
		},
		{
			name:    "daemon down + nohup → advise only",
			report:  rep(false, "", nil, nil, nil),
			backend: "nohup", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{adviseNoSupervisor},
		},
		{
			name:    "daemon down + systemd unreachable → advise only",
			report:  rep(false, "", nil, unitOK, lingerOn),
			backend: "systemd-user", available: false, goos: "linux", selfVer: self,
			want: []doctorFixKind{adviseNoSupervisor},
		},
		{
			name:    "two serve processes → restart",
			report:  rep(true, "v1.7.8", []serveProcess{{PID: 10}, {PID: 11}}, unitOK, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{fixRestartDaemon},
		},
		{
			name:    "deleted-exe serve process → restart",
			report:  rep(true, "v1.7.8", []serveProcess{{PID: 10, Deleted: true}}, unitOK, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{fixRestartDaemon},
		},
		{
			name:    "running-version skew → restart",
			report:  rep(true, "v1.7.6 (old, built …)", nil, unitOK, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{fixRestartDaemon},
		},
		{
			name:    "version skew + nohup → advise only",
			report:  rep(true, "v1.7.6", nil, nil, nil),
			backend: "nohup", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{adviseNoSupervisor},
		},
		{
			name:    "version skew suppressed for dev build",
			report:  rep(true, "v1.7.6", nil, unitOK, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: "v0.0.0-dev",
			want: nil,
		},
		{
			name:    "linger disabled → enable",
			report:  rep(true, "v1.7.8", nil, unitOK, lingerOff),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{fixEnableLinger},
		},
		{
			name:    "linger query errored → no linger action",
			report:  rep(true, "v1.7.8", nil, unitOK, lingerErrOff),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: nil,
		},
		{
			name:    "unit missing → refresh",
			report:  rep(true, "v1.7.8", nil, unitMissing, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{adviseUnitRefresh},
		},
		{
			name:    "unit lacks KillMode=process → refresh",
			report:  rep(true, "v1.7.8", nil, unitNoKill, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{adviseUnitRefresh},
		},
		{
			name:    "stale daemon + linger off → restart then linger, in order",
			report:  rep(true, "v1.7.6", nil, unitOK, lingerOff),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{fixRestartDaemon, fixEnableLinger},
		},
		{
			// A restart would wipe the cgroup (no KillMode=process) and drop
			// every session, so --fix must NOT auto-restart — it advises
			// fixing the unit first and does not also emit a separate refresh.
			name:    "stale daemon + unit lacks KillMode → advise refresh-first, no restart",
			report:  rep(true, "v1.7.6", nil, unitNoKill, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{adviseRestartNeedsUnit},
		},
		{
			name:    "stale (deleted exe) + unit missing → advise refresh-first, no restart",
			report:  rep(true, "v1.7.8", []serveProcess{{PID: 9, Deleted: true}}, unitMissing, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{adviseRestartNeedsUnit},
		},
		{
			name:    "stale + no-KillMode + linger off → advise restart-needs-unit then linger",
			report:  rep(true, "v1.7.6", nil, unitNoKill, lingerOff),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{adviseRestartNeedsUnit, fixEnableLinger},
		},
		{
			// A DOWN daemon has no sessions to lose, so start it AND advise
			// the unit fix for the next restart.
			name:    "daemon down + unit missing → start then advise unit",
			report:  rep(false, "", nil, unitMissing, lingerOn),
			backend: "systemd-user", available: true, goos: "linux", selfVer: self,
			want: []doctorFixKind{fixStartDaemon, adviseUnitRefresh},
		},
		{
			name:    "launchd daemon down → start (no linger/unit on macOS)",
			report:  rep(false, "", nil, nil, nil),
			backend: "launchd", available: true, goos: "darwin", selfVer: self,
			want: []doctorFixKind{fixStartDaemon},
		},
		{
			name:    "linger off but backend not systemd-user → no linger action",
			report:  rep(true, "v1.7.8", nil, nil, lingerOff),
			backend: "launchd", available: true, goos: "darwin", selfVer: self,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kindsOf(planFixes(tc.report, tc.backend, tc.available, tc.goos, tc.selfVer))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("planFixes kinds = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLingerNeedsPrivilege(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"Failed to enable linger for user u: Interactive authentication required.", true},
		{"INTERACTIVE AUTHENTICATION REQUIRED", true}, // case-insensitive
		{"Access denied", true},
		{"Failed to enable linger: Permission denied", true},
		{"Not authorized to perform operation.", true},
		{"", false}, // e.g. loginctl not found — a genuine failure, not a denial
		{"Failed to enable linger: unexpected response from logind", false},
	}
	for _, tc := range cases {
		if got := lingerNeedsPrivilege(tc.out); got != tc.want {
			t.Errorf("lingerNeedsPrivilege(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// A down daemon is the headline failure: it must be the FIRST planned
// action so verification runs against a daemon we actually started.
func TestPlanFixesOrdersDaemonFirst(t *testing.T) {
	r := rep(false, "", nil, unitMissing, lingerOff)
	got := kindsOf(planFixes(r, "systemd-user", true, "linux", "v1.7.8"))
	want := []doctorFixKind{fixStartDaemon, adviseUnitRefresh, fixEnableLinger}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
