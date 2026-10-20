// Package config loads kestrel's runtime settings.
//
// Cloud Run delivers every secret as one JSON object mounted at
// /secrets/config.json by the cloud-run-app module. Nothing sensitive arrives
// through the environment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultSecretsPath is where the cloud-run-app module mounts the config secret.
const DefaultSecretsPath = "/secrets/config.json"

// Check is one declared dead-man's switch. Terraform is the source of truth for
// this list; kestrel reconciles its table to match on every boot.
type Check struct {
	Name          string `json:"name"`
	Token         string `json:"token"`
	PeriodSeconds int    `json:"period_seconds"`
	GraceSeconds  int    `json:"grace_seconds"`
}

// Config is the fully resolved runtime configuration.
type Config struct {
	DatabaseURL    string
	DiscordWebhook string
	Checks         []Check

	// Audience and ServiceAccount authorize the Cloud Scheduler sweep call.
	// Audience may be empty: the Cloud Run URL is only known after the service
	// exists, and the service is what mounts this secret, so Terraform cannot
	// put it here without a cycle. ServiceAccount is the gate instead.
	Audience       string
	ServiceAccount string

	Port string
}

// secrets mirrors the JSON written by the cloud-run-app module's secrets map.
type secrets struct {
	DatabaseURLTransaction string `json:"DATABASE_URL_TRANSACTION"`
	DiscordWebhook         string `json:"DISCORD_WEBHOOK"`
	Checks                 string `json:"CHECKS"`
	SweepAudience          string `json:"SWEEP_AUDIENCE"`
	SweepServiceAccount    string `json:"SWEEP_SERVICE_ACCOUNT"`
}

// Load reads the mounted secret and the environment, then validates both.
func Load(path string) (Config, error) {
	if path == "" {
		path = DefaultSecretsPath
	}

	raw, err := os.ReadFile(
		path,
	) //nolint:gosec // the path is operator-supplied config, not user input
	if err != nil {
		return Config{}, fmt.Errorf("read the secrets file %s: %w", path, err)
	}

	var s secrets
	if err := json.Unmarshal(raw, &s); err != nil {
		return Config{}, fmt.Errorf("parse the secrets file %s: %w", path, err)
	}

	checks, err := parseChecks(s.Checks)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		DatabaseURL:    s.DatabaseURLTransaction,
		DiscordWebhook: s.DiscordWebhook,
		Checks:         checks,
		Audience:       s.SweepAudience,
		ServiceAccount: s.SweepServiceAccount,
		Port:           os.Getenv("PORT"),
	}
	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	return cfg, cfg.validate()
}

// parseChecks decodes the CHECKS entry. An absent entry is a valid empty list:
// kestrel still serves /health, it just has nothing to watch.
func parseChecks(encoded string) ([]Check, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}

	var checks []Check
	if err := json.Unmarshal([]byte(encoded), &checks); err != nil {
		return nil, fmt.Errorf("parse CHECKS: %w", err)
	}

	seenNames := make(map[string]bool, len(checks))
	seenTokens := make(map[string]bool, len(checks))

	for i, c := range checks {
		switch {
		case strings.TrimSpace(c.Name) == "":
			return nil, fmt.Errorf("check %d has no name", i)
		case strings.TrimSpace(c.Token) == "":
			return nil, fmt.Errorf("check %q has no token", c.Name)
		case c.PeriodSeconds <= 0:
			return nil, fmt.Errorf(
				"check %q has period_seconds %d, which must be above zero",
				c.Name,
				c.PeriodSeconds,
			)
		case c.GraceSeconds < 0:
			return nil, fmt.Errorf(
				"check %q has a negative grace_seconds %d",
				c.Name,
				c.GraceSeconds,
			)
		case seenNames[c.Name]:
			return nil, fmt.Errorf("check %q is declared twice", c.Name)
		case seenTokens[c.Token]:
			// Two checks sharing a token makes their ping URLs ambiguous.
			return nil, fmt.Errorf("check %q reuses another check's token", c.Name)
		}

		seenNames[c.Name] = true
		seenTokens[c.Token] = true
	}

	return checks, nil
}

func (c Config) validate() error {
	var errs []error

	if strings.TrimSpace(c.DatabaseURL) == "" {
		errs = append(errs, errors.New("DATABASE_URL_TRANSACTION is empty"))
	}

	if strings.TrimSpace(c.DiscordWebhook) == "" {
		errs = append(errs, errors.New("DISCORD_WEBHOOK is empty"))
	} else if !strings.HasPrefix(c.DiscordWebhook, "https://") {
		errs = append(errs, errors.New("DISCORD_WEBHOOK must be an https URL"))
	}

	// Without this the sweep endpoint cannot tell Cloud Scheduler from the
	// public internet, and the service is reachable by allUsers.
	if strings.TrimSpace(c.ServiceAccount) == "" {
		errs = append(errs, errors.New("SWEEP_SERVICE_ACCOUNT is empty"))
	}

	return errors.Join(errs...)
}
