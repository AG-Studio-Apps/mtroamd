module github.com/AG-Studio-Apps/mtroamd

go 1.26.3

// Patched toolchain: pulls in the go1.26.11 stdlib fixes for
// GO-2026-6859 (net/http MaxBytesReader header race), GO-2026-6809
// (crypto/x509 compromised-intermediate distrust — reachable via
// release.ChecksumOf's cert verification), GO-2026-6607 (crypto/tls
// malformed ECH outer extension — reachable via the QUIC listener's
// TLS handshake, the one server-facing advisory in this batch), and
// the GO-2026-6603/05/08/10/13/17 net/http HTTP/1 and HTTP/2 fixes —
// all call-graph-reachable per govulncheck, mostly via the release
// fetcher's HTTP client. Supersedes the earlier go1.26.5 bump for
// GO-2026-5856 (crypto/tls ECH privacy leak) + GO-2026-4970
// (os.Root symlink escape). The `go` directive stays at the language
// minimum so GOTOOLCHAIN=local builds (Nix) keep working.
toolchain go1.26.11

require (
	github.com/creack/pty v1.1.24
	github.com/fxamacker/cbor/v2 v2.9.2
	github.com/quic-go/quic-go v0.59.1
	golang.org/x/crypto v0.58.0
	golang.org/x/sys v0.49.0
	golang.org/x/term v0.47.0
)

require (
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/net v0.62.0 // indirect
)
