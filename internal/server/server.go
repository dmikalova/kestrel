// Package server exposes kestrel over HTTP.
//
// Three routes: a liveness probe, the ping that jobs call, and the sweep that
// Cloud Scheduler calls. The service is public, so the sweep authenticates.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/api/idtoken"

	"github.com/dmikalova/kestrel/internal/discord"
	"github.com/dmikalova/kestrel/internal/store"
)

// notifyTimeout bounds a Discord post so one slow webhook cannot hold a request.
const notifyTimeout = 30 * time.Second

// Server wires the store and the notifier to the routes.
type Server struct {
	store   *store.Store
	discord *discord.Client
	log     *slog.Logger

	// audience and serviceAccount are the two facts that identify Cloud Scheduler.
	audience       string
	serviceAccount string

	// validate is swapped in tests. It verifies a Google-signed ID token.
	validate func(ctx context.Context, token, audience string) (*idtoken.Payload, error)
}

// Options configures a Server.
type Options struct {
	Store          *store.Store
	Discord        *discord.Client
	Logger         *slog.Logger
	Audience       string
	ServiceAccount string
}

// New returns a Server.
func New(o Options) *Server {
	return &Server{
		store:          o.Store,
		discord:        o.Discord,
		log:            o.Logger,
		audience:       o.Audience,
		serviceAccount: o.ServiceAccount,
		validate:       idtoken.Validate,
	}
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /ping/{token}", s.handlePing)
	mux.HandleFunc("GET /ping/{token}", s.handlePing)
	mux.HandleFunc("POST /api/sweep", s.handleSweep)

	return mux
}

// handleHealth is liveness only. It deliberately does not touch the database:
// a database outage must not also make Cloud Run tear down the instance that
// reports the outage.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// handlePing records that a job finished. den calls this with an empty body.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	check, previous, err := s.store.RecordPing(r.Context(), token)
	if err != nil {
		if errors.Is(err, store.ErrUnknownToken) {
			// The token is a credential, so it is never echoed or logged.
			s.log.WarnContext(r.Context(), "ping with an unrecognized token")
			http.Error(w, "unknown check", http.StatusNotFound)

			return
		}

		s.log.ErrorContext(r.Context(), "record the ping", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	s.log.InfoContext(r.Context(), "ping", "check", check.Name, "previous_state", previous)

	if previous == "down" {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), notifyTimeout)
		defer cancel()

		if err := s.discord.Up(ctx, check.Name); err != nil {
			// The state change is already committed. Losing the announcement is
			// worse than silence, but it is not worth failing the job's ping.
			s.log.ErrorContext(ctx, "announce the recovery", "check", check.Name, "error", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleSweep moves overdue checks to down and announces each one.
func (s *Server) handleSweep(w http.ResponseWriter, r *http.Request) {
	if err := s.authorizeSweep(r); err != nil {
		s.log.WarnContext(r.Context(), "rejected a sweep", "error", err)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	overdue, err := s.store.ClaimOverdue(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "claim the overdue checks", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	for _, c := range overdue {
		s.log.WarnContext(
			r.Context(),
			"check is down",
			"check",
			c.Name,
			"last_ping_at",
			c.LastPingAt,
		)

		if err := s.discord.Down(r.Context(), c.Name, c.LastPingAt, c.Deadline()); err != nil {
			s.log.ErrorContext(r.Context(), "announce the outage", "check", c.Name, "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"down": len(overdue)})
}

// authorizeSweep accepts only a Google-signed ID token that belongs to
// kestrel's scheduler service account.
func (s *Server) authorizeSweep(r *http.Request) error {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(raw) == "" {
		return errors.New("no bearer token")
	}

	// Validate checks the Google signature and the expiry. It checks the
	// audience too, but only when one is configured, which it usually is not:
	// see the comment on config.Config.Audience.
	p, err := s.validate(r.Context(), raw, s.audience)
	if err != nil {
		return fmt.Errorf("validate the ID token: %w", err)
	}

	// A valid Google token is not enough. Any Google account can mint one, so
	// the caller's identity is what actually authorizes the sweep.
	email, _ := p.Claims["email"].(string)
	if email != s.serviceAccount {
		return errors.New("the token belongs to another service account")
	}

	if verified, _ := p.Claims["email_verified"].(bool); !verified {
		return errors.New("the token's email is not verified")
	}

	return nil
}
