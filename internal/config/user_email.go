package config

import "strings"

const (
	DefaultUserEmailVerificationTokenTTL = "24h"
	DefaultUserEmailResetTokenTTL        = "30m"
	DefaultUserEmailSMTPPort             = 587
)

// NormalizeUserEmailConfig applies stable defaults without deciding whether the feature is usable.
func (cfg *Config) NormalizeUserEmailConfig() {
	if cfg == nil {
		return
	}
	cfg.UserEmail.PublicUserURL = strings.TrimSpace(cfg.UserEmail.PublicUserURL)
	cfg.UserEmail.FromAddress = strings.TrimSpace(cfg.UserEmail.FromAddress)
	cfg.UserEmail.FromName = strings.TrimSpace(cfg.UserEmail.FromName)
	cfg.UserEmail.Sender.Type = strings.ToLower(strings.TrimSpace(cfg.UserEmail.Sender.Type))
	cfg.UserEmail.Sender.SMTP.Host = strings.TrimSpace(cfg.UserEmail.Sender.SMTP.Host)
	cfg.UserEmail.Sender.SMTP.Username = strings.TrimSpace(cfg.UserEmail.Sender.SMTP.Username)
	cfg.UserEmail.Sender.SMTP.PasswordEnv = strings.TrimSpace(cfg.UserEmail.Sender.SMTP.PasswordEnv)
	if cfg.UserEmail.Sender.SMTP.Port <= 0 {
		cfg.UserEmail.Sender.SMTP.Port = DefaultUserEmailSMTPPort
	}
	cfg.UserEmail.VerificationTokenTTL = strings.TrimSpace(cfg.UserEmail.VerificationTokenTTL)
	if cfg.UserEmail.VerificationTokenTTL == "" {
		cfg.UserEmail.VerificationTokenTTL = DefaultUserEmailVerificationTokenTTL
	}
	cfg.UserEmail.ResetTokenTTL = strings.TrimSpace(cfg.UserEmail.ResetTokenTTL)
	if cfg.UserEmail.ResetTokenTTL == "" {
		cfg.UserEmail.ResetTokenTTL = DefaultUserEmailResetTokenTTL
	}
}
