package usermail

import (
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

type ResolvedConfig struct {
	PublicUserURL   *url.URL
	FromAddress     string
	FromName        string
	SMTP            SMTPConfig
	VerificationTTL time.Duration
	ResetTTL        time.Duration
}

type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	StartTLS bool
	Timeout  time.Duration
}

// ResolveConfig validates the current runtime mail configuration.
func ResolveConfig(cfg *appconfig.Config) (ResolvedConfig, error) {
	if cfg == nil || !cfg.UserEmail.Enabled {
		return ResolvedConfig{}, fmt.Errorf("user email is disabled")
	}
	current := cfg.UserEmail
	publicURL, errURL := url.Parse(strings.TrimSpace(current.PublicUserURL))
	if errURL != nil || publicURL == nil || publicURL.IsAbs() == false || publicURL.Host == "" {
		return ResolvedConfig{}, fmt.Errorf("public user url must be absolute")
	}
	if publicURL.Scheme != "https" && !(publicURL.Scheme == "http" && isLoopbackHost(publicURL.Hostname())) {
		return ResolvedConfig{}, fmt.Errorf("public user url must use https")
	}
	if publicURL.User != nil {
		return ResolvedConfig{}, fmt.Errorf("public user url must not contain user information")
	}
	publicURL.RawQuery = ""
	publicURL.Fragment = ""

	fromAddress := strings.TrimSpace(current.FromAddress)
	parsedFrom, errFrom := mail.ParseAddress(fromAddress)
	if errFrom != nil || parsedFrom == nil || parsedFrom.Name != "" || !strings.EqualFold(parsedFrom.Address, fromAddress) {
		return ResolvedConfig{}, fmt.Errorf("from address is invalid")
	}
	fromName := strings.TrimSpace(current.FromName)
	if !validMailDisplayName(fromName) {
		return ResolvedConfig{}, fmt.Errorf("from name is invalid")
	}
	if strings.ToLower(strings.TrimSpace(current.Sender.Type)) != "smtp" {
		return ResolvedConfig{}, fmt.Errorf("smtp sender is required")
	}
	host := strings.TrimSpace(current.Sender.SMTP.Host)
	if host == "" {
		return ResolvedConfig{}, fmt.Errorf("smtp host is required")
	}
	port := current.Sender.SMTP.Port
	if port <= 0 || port > 65535 {
		return ResolvedConfig{}, fmt.Errorf("smtp port is invalid")
	}
	if port == 465 {
		return ResolvedConfig{}, fmt.Errorf("smtp implicit tls on port 465 is not supported")
	}
	if !current.Sender.SMTP.StartTLS && !isLoopbackHost(host) {
		return ResolvedConfig{}, fmt.Errorf("smtp starttls is required for non-loopback hosts")
	}
	password := ""
	passwordEnv := strings.TrimSpace(current.Sender.SMTP.PasswordEnv)
	if passwordEnv != "" {
		password = os.Getenv(passwordEnv)
	}
	username := strings.TrimSpace(current.Sender.SMTP.Username)
	if username != "" && passwordEnv == "" {
		return ResolvedConfig{}, fmt.Errorf("smtp password environment variable is required")
	}
	if username != "" && password == "" {
		return ResolvedConfig{}, fmt.Errorf("smtp password environment variable is empty")
	}
	verificationTTL, errVerificationTTL := time.ParseDuration(strings.TrimSpace(current.VerificationTokenTTL))
	if errVerificationTTL != nil || verificationTTL <= 0 {
		return ResolvedConfig{}, fmt.Errorf("verification token ttl is invalid")
	}
	resetTTL, errResetTTL := time.ParseDuration(strings.TrimSpace(current.ResetTokenTTL))
	if errResetTTL != nil || resetTTL <= 0 {
		return ResolvedConfig{}, fmt.Errorf("reset token ttl is invalid")
	}

	return ResolvedConfig{
		PublicUserURL:   publicURL,
		FromAddress:     fromAddress,
		FromName:        fromName,
		VerificationTTL: verificationTTL,
		ResetTTL:        resetTTL,
		SMTP: SMTPConfig{
			Host:     host,
			Port:     port,
			Username: username,
			Password: password,
			StartTLS: current.Sender.SMTP.StartTLS,
			Timeout:  20 * time.Second,
		},
	}, nil
}

func validMailDisplayName(value string) bool {
	if len([]rune(value)) > 128 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// Enabled reports whether all required mail settings are currently usable.
func Enabled(cfg *appconfig.Config) bool {
	_, errResolve := ResolveConfig(cfg)
	return errResolve == nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func smtpAddress(cfg SMTPConfig) string {
	return net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
}
