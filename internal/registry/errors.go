package registry

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Error classes for the UI. They decide what the user is told to do: fix credentials, fix the
// ref, or just wait and retry.
const (
	ErrAuth        = "auth"
	ErrNotFound    = "not_found"
	ErrRateLimited = "rate_limited"
	ErrNetwork     = "network"
	ErrOther       = "other"
)

// Classify sorts an error from Digest, Tags or CheckAll into "auth", "not_found",
// "rate_limited", "network" or "other" ("" for nil).
//
// Docker Hub answers 401 for repositories that don't exist when asked anonymously, so a typo in
// a Hub ref shows up as "auth", like the "pull access denied" docker prints for it.
func Classify(err error) string {
	if err == nil {
		return ""
	}
	var te *transport.Error
	if errors.As(err, &te) {
		// The registry's error codes are more specific than the status, when there is a body.
		for _, d := range te.Errors {
			switch d.Code {
			case transport.UnauthorizedErrorCode, transport.DeniedErrorCode:
				return ErrAuth
			case transport.ManifestUnknownErrorCode, transport.NameUnknownErrorCode:
				return ErrNotFound
			case transport.TooManyRequestsErrorCode:
				return ErrRateLimited
			}
		}
		switch {
		case te.StatusCode == http.StatusUnauthorized, te.StatusCode == http.StatusForbidden:
			return ErrAuth
		case te.StatusCode == http.StatusNotFound:
			return ErrNotFound
		case te.StatusCode == http.StatusTooManyRequests:
			return ErrRateLimited
		case te.StatusCode >= 500:
			return ErrNetwork // registry or something in front of it is down; retry later
		}
		return ErrOther
	}
	var ne net.Error // includes *url.Error, DNS and dial errors, timeouts
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) {
		return ErrNetwork
	}
	return ErrOther
}
