package usermail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

var (
	errSMTPStartTLSUnsupported = errors.New("smtp server does not support STARTTLS")
	errSMTPAuthUnsupported     = errors.New("smtp server does not support authentication")
)

type Message struct {
	To      string
	Subject string
	Text    string
}

type Sender interface {
	Send(context.Context, Message) error
}

type SMTPSender struct {
	config      SMTPConfig
	fromAddress string
	fromName    string
	tlsConfig   *tls.Config
}

func NewSMTPSender(cfg ResolvedConfig) *SMTPSender {
	return &SMTPSender{
		config:      cfg.SMTP,
		fromAddress: cfg.FromAddress,
		fromName:    cfg.FromName,
	}
}

func (s *SMTPSender) Send(ctx context.Context, message Message) error {
	if s == nil {
		return fmt.Errorf("smtp sender is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	to, errTo := mail.ParseAddress(strings.TrimSpace(message.To))
	if errTo != nil || to == nil || to.Name != "" {
		return fmt.Errorf("recipient address is invalid")
	}
	if containsHeaderBreak(message.Subject) {
		return fmt.Errorf("subject is invalid")
	}
	timeout := s.config.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, errDial := dialer.DialContext(ctx, "tcp", smtpAddress(s.config))
	if errDial != nil {
		return errDial
	}
	defer func() { _ = conn.Close() }()
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopCancellation()
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	client, errClient := smtp.NewClient(conn, s.config.Host)
	if errClient != nil {
		return errClient
	}
	defer func() { _ = client.Close() }()
	if s.config.StartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errSMTPStartTLSUnsupported
		}
		tlsConfig := &tls.Config{ServerName: s.config.Host, MinVersion: tls.VersionTLS12}
		if s.tlsConfig != nil {
			tlsConfig = s.tlsConfig.Clone()
			if tlsConfig.ServerName == "" {
				tlsConfig.ServerName = s.config.Host
			}
			if tlsConfig.MinVersion < tls.VersionTLS12 {
				tlsConfig.MinVersion = tls.VersionTLS12
			}
		}
		if errTLS := client.StartTLS(tlsConfig); errTLS != nil {
			return errTLS
		}
	}
	if s.config.Username != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return errSMTPAuthUnsupported
		}
		auth := smtp.PlainAuth("", s.config.Username, s.config.Password, s.config.Host)
		if errAuth := client.Auth(auth); errAuth != nil {
			return errAuth
		}
	}
	if errMail := client.Mail(s.fromAddress); errMail != nil {
		return errMail
	}
	if errRecipient := client.Rcpt(to.Address); errRecipient != nil {
		return errRecipient
	}
	writer, errData := client.Data()
	if errData != nil {
		return errData
	}
	if _, errWrite := io.WriteString(writer, s.renderMessage(to.Address, message)); errWrite != nil {
		_ = writer.Close()
		return errWrite
	}
	if errClose := writer.Close(); errClose != nil {
		return errClose
	}
	if errQuit := client.Quit(); errQuit != nil {
		log.WithFields(log.Fields{
			"smtp_host":  s.config.Host,
			"smtp_port":  s.config.Port,
			"error_type": fmt.Sprintf("%T", errQuit),
		}).Debug("smtp message accepted but connection cleanup failed")
	}
	return nil
}

func (s *SMTPSender) renderMessage(to string, message Message) string {
	from := (&mail.Address{Name: s.fromName, Address: s.fromAddress}).String()
	subject := mime.QEncoding.Encode("utf-8", strings.TrimSpace(message.Subject))
	return strings.Join([]string{
		"From: " + from,
		"To: " + (&mail.Address{Address: to}).String(),
		"Subject: " + subject,
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
		"",
		normalizeSMTPLineEndings(message.Text),
		"",
	}, "\r\n")
}

// normalizeSMTPLineEndings makes the rendered message RFC-compliant before
// net/smtp applies dot-stuffing and writes the DATA payload.
func normalizeSMTPLineEndings(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.ReplaceAll(value, "\n", "\r\n")
}

func containsHeaderBreak(value string) bool {
	return strings.ContainsAny(value, "\r\n")
}
