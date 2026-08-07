package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/quic-go/quic-go"

	"github.com/AG-Studio-Apps/mtroamd/internal/ipc"
	"github.com/AG-Studio-Apps/mtroamd/internal/protocol"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// ★★ Reattach must provoke the APP to repaint, not fabricate a frame from the model.
//
// A same-geometry reattach raises no SIGWINCH (a no-op SetSize signals nobody), so the app
// never redraws and the client is handed a frame the daemon synthesised from its own screen
// model. Everything fragile in this subsystem exists to judge whether that synthesis can be
// trusted. Rotating the phone fixes the symptom precisely because it changes the geometry
// and the app redraws itself.
//
// This drives the real thing: a real vim, a real QUIC attach, over the real transport. It
// asserts the daemon STOOD ASIDE (no synthesised frame on the wire) and that what the client
// received is the app's own repaint reaching the bottom row of the session geometry.
//
// Without the bounce this fails on the first assertion: the daemon ships its synthetic
// frame, which is recognisable by the preamble only Screen.Repaint emits.
func TestSameGeometryReattachProvokesAnAppRepaint(t *testing.T) {
	if _, err := exec.LookPath("vim"); err != nil {
		t.Skip("vim not installed; this test drives a real TUI on purpose")
	}
	_, c, cleanup := startDaemon(t)
	defer cleanup()

	const rows, cols = 21, 40
	dir := t.TempDir()
	// laststatus=2 pins a status line to the bottom row, and `normal G` parks the cursor
	// at the end so the frame is bottom-anchored the way an agent TUI is.
	script := "seq 1 200 > " + dir + "/f.txt; TERM=xterm-256color vim -u NONE -N " +
		"-c 'set laststatus=2' -c 'normal G' " + dir + "/f.txt"

	resp, err := c.Allocate(context.Background(), ipc.AllocateRequest{
		SessionID: "new", Rows: rows, Cols: cols,
		Shell: "/bin/sh", Exec: []string{"-c", script},
	})
	if err != nil || !resp.Ok {
		t.Fatalf("allocate: %v %s %s", err, resp.Err, resp.Msg)
	}
	// Let vim paint and the daemon's screen model settle.
	time.Sleep(3 * time.Second)

	// Reattach at the SAME geometry, exactly as the iOS client does (rows=0/cols=0 so the
	// attach itself does not resize).
	re, err := c.Allocate(context.Background(), ipc.AllocateRequest{SessionID: resp.SessionID})
	if err != nil || !re.Ok {
		t.Fatalf("reattach allocate: %v %s", err, re.Err)
	}
	painted, wedges := attachAndCollect(t, re, 0, 0)

	if len(painted) == 0 {
		t.Fatal("the client received nothing at all on reattach")
	}

	// ★ ASSERTION 1: the daemon must NOT have shipped a synthesised frame. This preamble
	// is emitted by Screen.Repaint and by nothing else.
	synthetic := []byte("\x1b[m\x1b[r\x1b[?6l\x1b[H\x1b[2J")
	if bytes.Contains(painted, synthetic) {
		t.Errorf("daemon shipped its SYNTHESISED frame instead of provoking the app to " +
			"repaint: the model is being trusted as authoritative on a same-geometry reattach")
	}

	// ★ ASSERTION 2: what arrived is a real repaint that reaches the bottom row of the
	// session geometry, so the client has a complete screen without needing a rotation.
	if got := highestRowAddressed(painted); got < rows {
		t.Errorf("the delivered repaint reaches row %d of %d; the bottom of the frame "+
			"(the input box / status line) is missing", got, rows)
	}

	// ★ ASSERTION 3: the bounce must not surface a false "may be wedged" banner. Two
	// resize arms fire in quick succession; cursor_row is closed by making them
	// back-to-back and `silent` by the suppression window.
	if wedges != 0 {
		t.Errorf("the bounce raised %d WedgeDetected frame(s): a false recovery banner "+
			"would appear in the app", wedges)
	}
}

// The pty geometry belongs to the session, not to one connection, so bouncing it reflows
// the app for every attached client. When anyone else is attached the daemon must leave it
// alone and fall back to the synthesised frame.
func TestReattachDoesNotBounceWhileAnotherClientIsAttached(t *testing.T) {
	if _, err := exec.LookPath("vim"); err != nil {
		t.Skip("vim not installed; this test drives a real TUI on purpose")
	}
	_, c, cleanup := startDaemon(t)
	defer cleanup()

	const rows, cols = 21, 40
	dir := t.TempDir()
	script := "seq 1 200 > " + dir + "/f.txt; TERM=xterm-256color vim -u NONE -N " +
		"-c 'set laststatus=2' -c 'normal G' " + dir + "/f.txt"
	resp, err := c.Allocate(context.Background(), ipc.AllocateRequest{
		SessionID: "new", Rows: rows, Cols: cols,
		Shell: "/bin/sh", Exec: []string{"-c", script},
	})
	if err != nil || !resp.Ok {
		t.Fatalf("allocate: %v %s", err, resp.Err)
	}
	time.Sleep(3 * time.Second)

	// A read-only observer stays attached for the duration. Read-only never displaces,
	// so it is still a live peer when the second client arrives.
	obs, err := c.Allocate(context.Background(), ipc.AllocateRequest{SessionID: resp.SessionID})
	if err != nil || !obs.Ok {
		t.Fatalf("observer allocate: %v %s", err, obs.Err)
	}
	stop := attachReadonlyAndHold(t, obs)
	defer stop()
	time.Sleep(400 * time.Millisecond) // let the observer register as a peer

	re, err := c.Allocate(context.Background(), ipc.AllocateRequest{SessionID: resp.SessionID})
	if err != nil || !re.Ok {
		t.Fatalf("reattach allocate: %v %s", err, re.Err)
	}
	painted, _ := attachAndCollect(t, re, 0, 0)

	synthetic := []byte("\x1b[m\x1b[r\x1b[?6l\x1b[H\x1b[2J")
	if !bytes.Contains(painted, synthetic) {
		t.Error("expected the daemon to fall back to its synthesised frame while another " +
			"client is attached, so the bounce cannot reflow the app underneath them")
	}
}

// attachReadonlyAndHold opens a read-only attach and keeps it alive until the returned
// func is called, so the session has a live peer.
func attachReadonlyAndHold(t *testing.T, resp ipc.AllocateResponse) func() {
	t.Helper()
	fp, err := hexToFP(resp.CertFP)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // verified via fingerprint below
		NextProtos:         []string{protocol.ALPN},
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no peer cert")
			}
			if sha256.Sum256(raw[0]) != fp {
				return errors.New("cert fingerprint mismatch")
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	conn, err := quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", resp.Port), cfg,
		&quic.Config{EnableDatagrams: true, MaxIdleTimeout: 20 * time.Second})
	if err != nil {
		cancel()
		t.Fatalf("observer dial: %v", err)
	}
	ctrl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	tok, _ := session.ParseAttachToken(resp.AttachToken)
	sid, _ := session.ParseSessionID(resp.SessionID)
	body, _ := protocol.MarshalAttach(protocol.Attach{
		V: 1, Token: tok[:], SessionID: sid[:], Rows: 0, Cols: 0,
		// ReplayBudget 0 on purpose: a budgeted attach would itself inject a synthesised
		// frame into the ring, and the assertion below (which looks for that frame) would
		// then pass no matter what the exclusive client's attach decided. Verified: with
		// a budget here, the test passes even with the peer guard removed.
		Mode: protocol.AttachModeReadonly, ReplayBudget: 0,
	})
	if err := protocol.WriteTaggedFrame(ctrl, protocol.FrameTypeControl, body); err != nil {
		cancel()
		t.Fatal(err)
	}
	if _, _, err := protocol.ReadTaggedFrame(ctrl); err != nil {
		cancel()
		t.Fatalf("observer AttachAck: %v", err)
	}
	return func() {
		_ = conn.CloseWithError(0, "")
		cancel()
	}
}

// highestRowAddressed returns the largest 1-based row any absolute CUP in b positions to.
func highestRowAddressed(b []byte) int {
	cup := regexp.MustCompile(`\x1b\[(\d+);\d*H`)
	max := 0
	for _, m := range cup.FindAllSubmatch(b, -1) {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n > max {
			max = n
		}
	}
	return max
}

// attachAndCollect runs the Attach handshake with the given requested geometry (0,0 asks
// for none) and returns the stdout payload the daemon delivers. Output frames arrive on the
// SAME bidirectional stream as the control frames.
func attachAndCollect(t *testing.T, resp ipc.AllocateResponse, wantRows, wantCols uint16) ([]byte, int) {
	t.Helper()
	fp, err := hexToFP(resp.CertFP)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // verified via fingerprint below
		NextProtos:         []string{protocol.ALPN},
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no peer cert")
			}
			if sha256.Sum256(raw[0]) != fp {
				return errors.New("cert fingerprint mismatch")
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", resp.Port), cfg,
		&quic.Config{EnableDatagrams: true, MaxIdleTimeout: 8 * time.Second})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseWithError(0, "") })

	ctrl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := session.ParseAttachToken(resp.AttachToken)
	sid, _ := session.ParseSessionID(resp.SessionID)
	body, err := protocol.MarshalAttach(protocol.Attach{
		V: 1, Token: tok[:], SessionID: sid[:],
		Rows: wantRows, Cols: wantCols, ReplayBudget: 600000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteTaggedFrame(ctrl, protocol.FrameTypeControl, body); err != nil {
		t.Fatal(err)
	}
	ft, respBody, err := protocol.ReadTaggedFrame(ctrl)
	if err != nil {
		t.Fatalf("read AttachAck: %v", err)
	}
	if ft != protocol.FrameTypeControl {
		t.Fatalf("first frame type = %d, want control", ft)
	}
	var ack protocol.AttachAck
	if err := cbor.Unmarshal(respBody, &ack); err != nil {
		t.Fatal(err)
	}
	if !ack.OK {
		t.Fatalf("AttachAck not ok: %s %s", ack.Err, ack.Msg)
	}

	// Collect for long enough to cover the app's SIGWINCH response. Real Claude answers in
	// ~13ms but Ink throttles renders to ~250ms, so a burst can be answered up to ~226ms
	// after the first signal; 2.5s is generous.
	var out []byte
	wedges := 0
	// Long enough to cover the `silent` wedge detector, which fires 2s after the last
	// resize arm if too few bytes flowed.
	_ = ctrl.SetReadDeadline(time.Now().Add(3500 * time.Millisecond))
	for {
		ft, fb, err := protocol.ReadTaggedFrame(ctrl)
		if err != nil {
			break
		}
		switch {
		case ft == protocol.FrameTypeStdout && len(fb) > 8:
			out = append(out, fb[8:]...) // strip the 8-byte seq prefix
		case ft == protocol.FrameTypeControl:
			if typ, err := protocol.PeekType(fb); err == nil && typ == protocol.TypeWedgeDetected {
				wedges++
			}
		}
	}
	return out, wedges
}
