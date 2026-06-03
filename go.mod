module github.com/AG-Studio-Apps/mtroamd

go 1.26.3

// Patched toolchain: GO-2026-5039 (net/textproto) + GO-2026-5037
// (crypto/x509), both fixed in go1.26.4. The `go` directive stays at
// the language minimum so GOTOOLCHAIN=local builds (Nix) keep working.
toolchain go1.26.4

require (
	github.com/creack/pty v1.1.24
	github.com/fxamacker/cbor/v2 v2.9.2
	github.com/quic-go/quic-go v0.59.0
	golang.org/x/crypto v0.52.0
	golang.org/x/sys v0.45.0
	golang.org/x/term v0.43.0
)

require (
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/net v0.55.0 // indirect
)
