package middleware

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// MaxBodyBytes caps an agent request's body: a real supervisor's largest
// message was 43 KB, so 4 MiB is generous headroom while bounding what an
// unauthenticated caller can make the server hold.
const MaxBodyBytes = 4 << 20

// DeclaredLength answers 413 at once, reading nothing, for a request whose
// Content-Length is over max: the stack's first check, before a concurrency
// slot is taken or a credential read. A body without a declared length
// (chunked) is capped while BodyLimit reads it, after the authentication.
// Its refusals come before the request has a Principal, so they are logged
// once a minute with their count (the route and the outcome; nothing of the
// request), by the clock now.
func DeclaredLength(max int64, now func() time.Time, logger *slog.Logger) Middleware {
	sampler := NewRefusalSampler(now)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength <= max {
				next.ServeHTTP(w, r)
				return
			}
			setOutcome(r.Context(), outcomeTooLarge)
			if log, count := sampler.Note(outcomeTooLarge); log {
				logger.Warn("agent request refused", "route", AgentRoute, "status", http.StatusRequestEntityTooLarge,
					"outcome", outcomeTooLarge, "refused_since_last_log", count)
			}
			WriteTooLarge(w)
		})
	}
}

// BodyReadTimeout is how long an authenticated request's body may take to
// arrive: a real supervisor's largest message was 43 KB, so 5 seconds is
// ample, and a body still unsent then is refused. Without it, a valid
// credential could hold a slot for the listener's whole 30 s ReadTimeout
// per request by never finishing its body.
const BodyReadTimeout = 5 * time.Second

// BodyLimit reads the request's body, at most max bytes, within timeout,
// and answers 413 past the size, whether its length is declared or it is
// chunked (capped while it is read). In the AgentStack it comes after the
// authentication and the organization's limits (reading it before the
// authentication would let unauthenticated uploads hold every concurrency
// slot and the memory their bodies took), so only an authenticated
// request's body is ever read, and only while its organization holds one of
// its slots.
//
// The read has a deadline, timeout from its start: the connection's read
// deadline, set through http.ResponseController (a socket's deadline is the
// kernel's, so it is the wall clock's, not the injected one), and cleared
// once the body is read, so the connection's background read (which net/http
// starts at the body's end, and which cancels the request's context when it
// fails) never hits it while the poll runs. A body not read in time answers
// 400 (outcome body_read_timeout). A read that failed keeps its deadline:
// net/http, before it writes the refusal, discards what is left of the body
// (up to 256 KiB), and that read then fails at once instead of waiting on a
// client that sends nothing more, and the connection closes after the
// refusal (what remains on the wire is never read as another request). A
// writer that can't set deadlines (a test recorder) is read without one.
//
// A `Content-Encoding: gzip` body is decompressed through the same cap (a
// gzip bomb stops at max bytes out); the handler then reads the plain
// bytes, the header removed. A body that can't be read, gzip that doesn't
// decompress, or another encoding answers 400. The supervisor 0.162 never
// compresses; the cap holds either way. Each refusal is one log line (the
// route, the outcome, and the request's ids when it has a Principal); a
// body whose client went away mid-read is request_cancelled, at Info.
func BodyLimit(max int64, timeout time.Duration, logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > max {
				refuseBody(w, r, logger, http.StatusRequestEntityTooLarge, outcomeTooLarge)
				return
			}
			controller := http.NewResponseController(w)
			deadlineSet := controller.SetReadDeadline(time.Now().Add(timeout)) == nil
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
			if deadlineSet && err == nil {
				_ = controller.SetReadDeadline(time.Time{})
			}
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				refuseBody(w, r, logger, http.StatusRequestEntityTooLarge, outcomeTooLarge)
				return
			case errors.Is(err, os.ErrDeadlineExceeded):
				// Before the cancelled check: net/http cancels the request's
				// context on any failed read, the deadline's included.
				refuseBody(w, r, logger, http.StatusBadRequest, outcomeBodyTimeout)
				return
			case err != nil:
				refuseBody(w, r, logger, http.StatusBadRequest, outcomeUnreadable)
				return
			}
			encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
			body := raw
			switch encoding {
			case "", "identity":
			case "gzip":
				zr, err := gzip.NewReader(bytes.NewReader(raw))
				if err != nil {
					refuseBody(w, r, logger, http.StatusBadRequest, outcomeUnreadable)
					return
				}
				body, err = io.ReadAll(io.LimitReader(zr, max+1))
				if err != nil {
					refuseBody(w, r, logger, http.StatusBadRequest, outcomeUnreadable)
					return
				}
				if int64(len(body)) > max {
					refuseBody(w, r, logger, http.StatusRequestEntityTooLarge, outcomeTooLarge)
					return
				}
			default:
				refuseBody(w, r, logger, http.StatusBadRequest, outcomeUnreadable)
				return
			}
			read := r.Clone(r.Context())
			read.Header.Del("Content-Encoding")
			read.Header.Set("Content-Length", strconv.Itoa(len(body)))
			read.ContentLength = int64(len(body))
			read.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, read)
		})
	}
}

// refuseBody answers a body refused: 413 or 400, with the span's outcome
// and one log line (the route, the outcome and the Principal's ids; nothing
// of the body). A request whose client went away (its context cancelled,
// and not by the read's deadline) is request_cancelled, at Info.
func refuseBody(w http.ResponseWriter, r *http.Request, logger *slog.Logger, status int, outcome string) {
	level := slog.LevelWarn
	if outcome != outcomeBodyTimeout && ClientGone(r.Context()) {
		outcome, level = outcomeCancelled, slog.LevelInfo
	}
	setOutcome(r.Context(), outcome)
	attrs := []any{"route", AgentRoute, "status", status, "outcome", outcome}
	if p, ok := PrincipalFrom(r.Context()); ok {
		attrs = append(attrs, "organization_id", p.OrganizationID.String(), "fleet_id", p.FleetID.String(),
			"token_id", p.TokenID.String())
	}
	message := "agent request refused"
	if level == slog.LevelInfo {
		message = "agent request abandoned by its client"
	}
	logger.Log(r.Context(), level, message, attrs...)
	if status == http.StatusRequestEntityTooLarge {
		WriteTooLarge(w)
		return
	}
	WriteBadRequest(w)
}
