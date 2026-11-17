package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"time"
)

// CheckHealth checks the local server for the container healthcheck command.
func CheckHealth(ctx context.Context, port string) (err error) {
	call := beginNetworkCall(ctx, nil, "healthcheck", "local_server")
	defer func() { call.finish(err) }()
	client := http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/healthz", nil)
	if err != nil {
		return err
	}
	call.phase = "transport"
	resp, err := client.Do(req)
	if err != nil {
		call.cause = networkTransportCause(err)
		return fmt.Errorf("healthcheck: %s", call.cause)
	}
	defer resp.Body.Close()
	call.status = resp.StatusCode
	call.phase = "http_status"
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: status %d", resp.StatusCode)
	}
	return nil
}

// networkCall logs only local labels and numeric diagnostics. Never pass URLs,
// headers, query values, bodies, or error strings from another system here.
type networkCall struct {
	ctx                              context.Context
	logger                           *slog.Logger
	service, operation, phase, cause string
	status                           int
	started                          time.Time
}

func beginNetworkCall(ctx context.Context, logger *slog.Logger, service, operation string) *networkCall {
	if logger == nil {
		logger, _ = ctx.Value(networkLoggerKey{}).(*slog.Logger)
	}
	if logger == nil {
		logger = slog.Default()
	}
	c := &networkCall{ctx: ctx, logger: logger, service: service, operation: operation, phase: "request", started: time.Now()}
	logger.InfoContext(ctx, "outbound request started", c.attrs()...)
	return c
}

type networkLoggerKey struct{}
type networkJobKey struct{}

func (c *networkCall) attrs() []any {
	attrs := []any{"service", c.service, "operation", c.operation}
	if s, ok := c.ctx.Value(contextKey{}).(requestState); ok && s.ID != "" {
		attrs = append(attrs, "request_id", s.ID)
	}
	if id, ok := c.ctx.Value(networkJobKey{}).(string); ok {
		attrs = append(attrs, "job_id", id)
	}
	return attrs
}

func (c *networkCall) finish(err error) {
	level, outcome := slog.LevelInfo, "success"
	if err != nil {
		level, outcome = slog.LevelWarn, "failed"
		if c.cause == "" {
			c.cause = c.phase
		}
		if errors.Is(err, errLookupNotFound) {
			level, outcome = slog.LevelInfo, "not_found"
		}
	}
	attrs := append(c.attrs(), "outcome", outcome, "phase", c.phase, "cause", c.cause, "http_status", c.status, "duration", time.Since(c.started))
	c.logger.Log(c.ctx, level, "outbound request finished", attrs...)
}

func networkTransportCause(err error) string {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var invalidCert x509.CertificateInvalidError
	var hostname x509.HostnameError
	var network net.Error
	switch {
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &cert), errors.As(err, &unknownAuthority), errors.As(err, &invalidCert), errors.As(err, &hostname):
		return "tls_certificate"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	default:
		return "connection"
	}
}
