package bridge

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"time"
)

// errUncertain means the server may have accepted the message even though
// Send failed, so it must not be retried.
var errUncertain = errors.New("smtp: no reply to end of data; the message may have been sent")

// Sender submits a message to an account's SMTP server.
type Sender interface {
	Send(s Smtp, from string, to []string, msg []byte) error
}

// SMTPSender submits over implicit TLS or STARTTLS with AUTH PLAIN.
type SMTPSender struct{ Timeout time.Duration }

func (ss SMTPSender) Send(s Smtp, from string, to []string, msg []byte) error {
	host, _, err := net.SplitHostPort(s.Address)
	if err != nil {
		return err
	}
	timeout := ss.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	tlsConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Timeout: 30 * time.Second}

	var conn net.Conn
	if s.Security == SecurityTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", s.Address, tlsConfig)
	} else {
		conn, err = dialer.Dial("tcp", s.Address)
	}
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return err
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	if s.Security == SecurityStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: server does not offer STARTTLS")
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	// PlainAuth refuses to send credentials over an unencrypted connection.
	if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("RCPT %s: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		if _, ok := errors.AsType[*textproto.Error](err); ok {
			return err
		}
		return fmt.Errorf("%w: %v", errUncertain, err)
	}
	// The server accepted the message; a failed QUIT must not cause a resend.
	_ = c.Quit()
	return nil
}

// isTransient reports whether an SMTP error is worth retrying: 4xx replies
// and network errors. 5xx replies and uncertain delivery are not.
func isTransient(err error) bool {
	if errors.Is(err, errUncertain) {
		return false
	}
	if tpErr, ok := errors.AsType[*textproto.Error](err); ok {
		return tpErr.Code >= 400 && tpErr.Code < 500
	}
	return true
}
