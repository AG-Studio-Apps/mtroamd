package transport

import (
	"context"
	"log/slog"
)

// LoggingHandler is the v0.0.x placeholder Handler. It logs each
// accepted connection's remote address, then closes with a
// "not implemented" application error. Replaced in a later commit
// by the real protocol handler that drives Attach / replay / stream
// demux. Used by tests + early bring-up; not on the hot path.
type LoggingHandler struct {
	Logger *slog.Logger
}

// HandleConnection implements Handler.
func (h *LoggingHandler) HandleConnection(ctx context.Context, c Conn) {
	logger := h.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "accepted connection",
		"remote", c.RemoteAddr().String(),
	)
	_ = c.CloseWithError(0, "not implemented yet")
}
