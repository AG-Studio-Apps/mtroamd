package transport

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

const (
	quicPortStateFile = "quic-port"
	tcpPortStateFile  = "tcp-port"
)

func readPortState(stateDir string) uint16 {
	return readPortStateFile(stateDir, quicPortStateFile)
}

func writePortState(stateDir string, port uint16) {
	writePortStateFile(stateDir, quicPortStateFile, port)
}

func readTCPPortState(stateDir string) uint16 {
	return readPortStateFile(stateDir, tcpPortStateFile)
}

func writeTCPPortState(stateDir string, port uint16) {
	writePortStateFile(stateDir, tcpPortStateFile, port)
}

func readPortStateFile(stateDir, filename string) uint16 {
	if stateDir == "" {
		return 0
	}
	path := filepath.Join(stateDir, filename)
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("transport: read port state",
				"path", path, "err", err)
		}
		return 0
	}
	n, err := strconv.ParseUint(string(bytes.TrimSpace(raw)), 10, 16)
	if err != nil || n == 0 {
		slog.Warn("transport: port state unparseable, ignoring",
			"path", path, "raw", string(raw), "err", err)
		return 0
	}
	return uint16(n)
}

func writePortStateFile(stateDir, filename string, port uint16) {
	if stateDir == "" {
		return
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		slog.Warn("transport: mkdir state dir for port state",
			"dir", stateDir, "err", err)
		return
	}
	path := filepath.Join(stateDir, filename)
	tmp, err := os.CreateTemp(stateDir, filename+".tmp-*")
	if err != nil {
		slog.Warn("transport: create temp file for port state",
			"dir", stateDir, "err", err)
		return
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := fmt.Fprintf(tmp, "%d\n", port); err != nil {
		slog.Warn("transport: write port state", "path", tmpPath, "err", err)
		cleanup()
		return
	}
	if err := tmp.Close(); err != nil {
		slog.Warn("transport: close port state tempfile",
			"path", tmpPath, "err", err)
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		slog.Warn("transport: rename port state", "from", tmpPath, "to", path, "err", err)
		_ = os.Remove(tmpPath)
		return
	}
}
