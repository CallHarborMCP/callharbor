package api

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/service"
)

// Headers the audio response always carries. Audio is private and never cached,
// and browsers must not sniff a type from it.
const (
	recordingCacheControl = "private, no-store"
	recordingNoSniff      = "nosniff"
	recordingContentWAV   = "audio/wav"
)

// getCallRecording serves one call's recording metadata. The response is the
// RecordingView the MCP get_recording tool also returns.
func (h *Handler) getCallRecording(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	res, err := h.svc.GetRecording(r.Context(), actor, r.PathValue("call_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// getCallRecordingAudio streams one call's WAV audio. The service opens and
// authenticates the file before anything is written, so a refusal still gets
// the JSON error envelope. Range handling, 206, and 416 come from
// http.ServeContent over the decrypting stream.
//
// A decryption failure after the first byte cannot change the status any more.
// ServeContent then stops copying with an error and the connection is closed
// without the rest of the body, so a truncated file is never served as if it
// were complete. The client sees a short body against Content-Length.
func (h *Handler) getCallRecordingAudio(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	rangeHeader := r.Header.Get("Range")
	open, err := h.svc.OpenRecordingAudio(r.Context(), actor, r.PathValue("call_id"), rangeHeader)
	if err != nil {
		h.fail(w, err)
		return
	}
	defer func() { _ = open.Close() }()

	name := callDownloadName(r.PathValue("call_id"))
	header := w.Header()
	header.Set("Content-Type", recordingContentWAV)
	header.Set("Cache-Control", recordingCacheControl)
	header.Set("X-Content-Type-Options", recordingNoSniff)
	header.Set("Accept-Ranges", "bytes")
	if r.URL.Query().Get("download") == "1" {
		header.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	} else {
		header.Set("Content-Disposition", `inline; filename="`+name+`"`)
	}
	// Zero modtime: no Last-Modified and no conditional requests. Audio is
	// immutable once finalized, but a validator would add cache state for no gain.
	http.ServeContent(w, r, name, time.Time{}, open.Audio)
	h.abortOnStreamError(open.Audio)
}

// abortOnStreamError ends the response when the audio stream failed during the
// copy. ServeContent has no error return, so the failure is read from the
// stream. The failure is logged server-side and the handler panics with
// http.ErrAbortHandler, which the server treats as a silent connection abort:
// the body stops short of its Content-Length, so the client sees a truncated
// response rather than bytes that did not authenticate.
func (h *Handler) abortOnStreamError(audio io.ReadSeeker) {
	s, ok := audio.(interface{ Failed() error })
	if !ok {
		return
	}
	if err := s.Failed(); err != nil {
		service.LogCause(h.logger, "rest", service.InternalWithCause(err))
		panic(http.ErrAbortHandler)
	}
}

// callDownloadName is the attachment file name for a call. The call id is
// sanitized to the id alphabet so it cannot break the quoted header value.
func callDownloadName(callID string) string {
	var b strings.Builder
	for _, c := range callID {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteRune('_')
		}
	}
	return "call-" + b.String() + ".wav"
}
