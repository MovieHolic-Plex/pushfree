// Package fcm implements the optional Firebase Cloud Messaging v1 delivery
// channel for pushfree.
//
// The channel is env-gated: it activates only when a service-account JSON
// path is supplied (config "fcm-credentials-file" / PUSHFREE_FCM_CREDENTIALS).
// With no path set the channel is disabled and the server boots normally;
// MaybeNew logs a single notice and returns a nil Client in that case.
//
// This package talks to the FCM v1 HTTP API directly
// (POST https://fcm.googleapis.com/v1/projects/{project}/messages:send) with a
// service-account OAuth2 bearer token minted via a self-signed RS256 JWT
// exchange. It uses only the Go standard library so the single static binary
// gains no heavyweight Google client dependency and the transport stays fully
// mockable with httptest. Credentials are read only from the external file
// path supplied by the caller, never from configuration values or literals
// (the ntfy #682 lesson: never hardcode or accept inline credentials).
//
// Messages are data-only (no "notification" block); the Android client
// (todo 30) renders them. Priority mapping: pushfree priority 1 and 2 map to
// Android "HIGH", everything else to "NORMAL".
//
// Error classification drives side effects:
//   - UNREGISTERED / INVALID_ARGUMENT -> the device's fcm_token is cleared via
//     DeviceTokenStore so it re-registers (terminal).
//   - QUOTA_EXCEEDED / TEMPORARY_BANNED -> Backoff state + classified log;
//     todo 21's retry scheduler consumes the SendResult to schedule a retry.
//   - HTTP 5xx without a specific code -> Backoff (transient).
//   - anything else -> Failed state + log.
//
// Delivery success is reported through the DeliveryRecorder seam, mirroring
// the hub DeliveryHook (todo 13) so delivered_at / receipt last_delivered_at
// are updated. Both DeviceTokenStore and DeliveryRecorder are local interfaces
// so the store package need not be imported here and todo 21 can plug in its
// own implementations.
package fcm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultEndpoint is the FCM v1 API root.
	defaultEndpoint = "https://fcm.googleapis.com"
	// tokenScope is the OAuth2 scope required to send FCM v1 messages.
	tokenScope = "https://www.googleapis.com/auth/firebase.messaging"
)

// Outbound is a fully-resolved message destined for one FCM registration
// token. MsgID is the per-recipient message row id (store.Message.ID) so a
// successful send can be reported through DeliveryRecorder; DeviceID is the
// pushfree device identifier so a terminal failure can clear its token.
type Outbound struct {
	MsgID    int64
	DeviceID string
	Token    string
	Priority int // pushfree priority [-2, 2]
	Data     map[string]string
}

// DeliveryState classifies the outcome of a single FCM send.
type DeliveryState int

const (
	// StateDelivered means FCM accepted the message (HTTP 2xx).
	StateDelivered DeliveryState = iota
	// StateNotRegistered means the token is invalid (UNREGISTERED or
	// INVALID_ARGUMENT); the device token has been cleared.
	StateNotRegistered
	// StateBackoff means a transient failure (QUOTA_EXCEEDED,
	// TEMPORARY_BANNED, or HTTP 5xx); the caller should retry later.
	StateBackoff
	// StateFailed means a permanent/uncategorized failure.
	StateFailed
)

// String returns a lowercase stable label for logs and assertions.
func (s DeliveryState) String() string {
	switch s {
	case StateDelivered:
		return "delivered"
	case StateNotRegistered:
		return "not_registered"
	case StateBackoff:
		return "backoff"
	case StateFailed:
		return "failed"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// SendResult is returned by Send. It carries the classification plus the raw
// FCM reason text so todo 21's retry scheduler and operators see real bodies
// (no misleading success output).
type SendResult struct {
	State  DeliveryState
	Out    Outbound
	Reason string // raw FCM error detail/classification, "" on success
	HTTP   int    // HTTP status code FCM returned
}

// TokenSource mints an OAuth2 bearer token for the FCM v1 API. The production
// implementation is serviceAccountTokenSource (RS256 JWT exchange); tests
// inject a stub to skip the network.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// DeliveryRecorder is the delivery-success seam. On a successful FCM send the
// channel calls RecordDelivery so delivered_at and the receipt's
// last_delivered_at are updated. This mirrors the hub DeliveryHook (todo 13);
// defining it locally avoids importing the hub package and lets todo 21 plug
// in its own implementation.
type DeliveryRecorder interface {
	RecordDelivery(ctx context.Context, messageID int64) error
}

// DeviceTokenStore is the narrow store seam for token invalidation. It
// matches store.DeviceRepo.ClearFCMToken (todo 16) without importing the
// store package.
type DeviceTokenStore interface {
	ClearFCMToken(ctx context.Context, deviceID string) error
}

// Options configures the FCM v1 client. CredentialsPath is required to enable
// the channel; everything else has sensible defaults and exists for test
// injection.
type Options struct {
	// CredentialsPath is the path to a Google service-account JSON file.
	// Required to enable the channel.
	CredentialsPath string
	// HTTPClient overrides the transport. Defaults to http.DefaultClient.
	// Tests point this at an httptest.Server.
	HTTPClient *http.Client
	// Tokens overrides the token source. Defaults to a service-account JWT
	// source built from CredentialsPath.
	Tokens TokenSource
	// Endpoint overrides the FCM API root. Defaults to the public endpoint.
	Endpoint string
	// Recorder receives delivery-success notifications. Optional.
	Recorder DeliveryRecorder
	// Devices clears registration tokens on terminal failures. Optional.
	Devices DeviceTokenStore
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Client is a configured FCM v1 delivery channel. A nil Client means the
// channel is disabled (obtained from MaybeNew with no credentials); callers
// simply skip FCM in that case.
type Client struct {
	projectID string
	endpoint  string
	http      *http.Client
	tokens    TokenSource
	recorder  DeliveryRecorder
	devices   DeviceTokenStore
	log       *slog.Logger
}

// MaybeNew loads the channel from configuration. If credentialsPath is empty
// it logs a single notice that the channel is disabled and returns (nil, nil)
// so the caller boots normally. If the path is set but the file cannot be
// read or parsed, it returns the error so a misconfigured channel fails loud
// rather than silently.
func MaybeNew(ctx context.Context, credentialsPath string, log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	if credentialsPath == "" {
		log.Info("fcm: delivery channel disabled (no credentials configured)")
		return nil, nil
	}
	return New(ctx, Options{CredentialsPath: credentialsPath, Logger: log})
}

// New loads the service-account credentials and returns a ready Client. A
// non-empty CredentialsPath, a parseable key, and a non-empty project_id are
// required.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.CredentialsPath == "" {
		return nil, fmt.Errorf("fcm: credentials path is empty")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.Endpoint == "" {
		opts.Endpoint = defaultEndpoint
	}
	sa, err := loadServiceAccount(opts.CredentialsPath)
	if err != nil {
		return nil, fmt.Errorf("fcm: load credentials: %w", err)
	}
	if sa.ProjectID == "" {
		return nil, fmt.Errorf("fcm: credentials %q: project_id is empty", opts.CredentialsPath)
	}
	if sa.ClientEmail == "" {
		return nil, fmt.Errorf("fcm: credentials %q: client_email is empty", opts.CredentialsPath)
	}
	c := &Client{
		projectID: sa.ProjectID,
		endpoint:  opts.Endpoint,
		http:      opts.HTTPClient,
		recorder:  opts.Recorder,
		devices:   opts.Devices,
		log:       opts.Logger,
	}
	if opts.Tokens != nil {
		c.tokens = opts.Tokens
	} else {
		ts, err := newServiceAccountTokenSource(opts.HTTPClient, sa)
		if err != nil {
			return nil, fmt.Errorf("fcm: build token source: %w", err)
		}
		c.tokens = ts
	}
	return c, nil
}

// Enabled reports whether the channel is active. A nil Client is disabled.
func (c *Client) Enabled() bool { return c != nil }

// Send delivers one data-only message to FCM v1 and classifies the result.
// It never returns a non-nil error for an FCM-level failure: those are
// encoded in SendResult.State so the caller (retry scheduler) can branch
// uniformly. A non-nil error means the call could not be completed at all
// (token minting or transport failure), in which case State is StateFailed.
func (c *Client) Send(ctx context.Context, out Outbound) (SendResult, error) {
	result := SendResult{Out: out}
	if out.Token == "" {
		result.State = StateFailed
		result.Reason = "empty fcm token"
		c.log.Warn("fcm: send failed", "device_id", out.DeviceID, "msg_id", out.MsgID, "reason", result.Reason)
		return result, nil
	}

	token, err := c.tokens.Token(ctx)
	if err != nil {
		result.State = StateFailed
		result.Reason = "token mint: " + err.Error()
		c.log.Warn("fcm: send failed", "device_id", out.DeviceID, "msg_id", out.MsgID, "reason", result.Reason)
		return result, nil
	}

	body, err := json.Marshal(fcmRequest{Message: fcmMessage{
		Token:   out.Token,
		Android: &androidConfig{Priority: androidPriority(out.Priority)},
		Data:    out.Data,
	}})
	if err != nil {
		result.State = StateFailed
		result.Reason = "marshal: " + err.Error()
		return result, nil
	}

	url := c.endpoint + "/v1/projects/" + c.projectID + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		result.State = StateFailed
		result.Reason = "new request: " + err.Error()
		return result, nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.http.Do(req)
	if err != nil {
		result.State = StateFailed
		result.Reason = "transport: " + err.Error()
		c.log.Warn("fcm: send failed", "device_id", out.DeviceID, "msg_id", out.MsgID, "reason", result.Reason)
		return result, nil
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	result.HTTP = resp.StatusCode

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.State = StateDelivered
		if c.recorder != nil {
			if rerr := c.recorder.RecordDelivery(ctx, out.MsgID); rerr != nil {
				c.log.Warn("fcm: delivery record failed", "device_id", out.DeviceID, "msg_id", out.MsgID, "err", rerr)
			}
		}
		c.log.Info("fcm: delivered", "device_id", out.DeviceID, "msg_id", out.MsgID)
		return result, nil
	}

	state, reason := classifyError(resp.StatusCode, respBody)
	result.State = state
	result.Reason = reason
	switch state {
	case StateNotRegistered:
		c.log.Warn("fcm: token not registered; invalidating",
			"device_id", out.DeviceID, "msg_id", out.MsgID, "state", state.String(), "reason", reason)
		if c.devices != nil {
			if cerr := c.devices.ClearFCMToken(ctx, out.DeviceID); cerr != nil {
				c.log.Warn("fcm: clear token failed", "device_id", out.DeviceID, "err", cerr)
			}
		}
	case StateBackoff:
		c.log.Warn("fcm: transient failure; backoff",
			"device_id", out.DeviceID, "msg_id", out.MsgID, "state", state.String(), "reason", reason)
	default:
		c.log.Warn("fcm: send failed",
			"device_id", out.DeviceID, "msg_id", out.MsgID, "state", state.String(), "reason", reason)
	}
	return result, nil
}

// androidPriority maps a pushfree priority to the FCM Android priority.
// pushfree priority 1 and 2 (emergency/high) map to "HIGH"; everything else
// (normal, low, lowest) maps to "NORMAL".
func androidPriority(pushfreePriority int) string {
	if pushfreePriority >= 1 {
		return "HIGH"
	}
	return "NORMAL"
}

// fcmRequest is the FCM v1 messages:send request body.
type fcmRequest struct {
	Message fcmMessage `json:"message"`
}

// fcmMessage is the data-only message. There is intentionally no
// "notification" block: pushfree delivers data so the client renders it.
type fcmMessage struct {
	Token   string            `json:"token"`
	Android *androidConfig    `json:"android,omitempty"`
	Data    map[string]string `json:"data,omitempty"`
}

type androidConfig struct {
	Priority string `json:"priority"`
}

// errorResponse is the canonical Google RPC error envelope FCM returns on
// non-2xx. The FcmError detail carries the actionable errorCode.
type errorResponse struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []struct {
			Type      string `json:"@type"`
			ErrorCode string `json:"errorCode"`
		} `json:"details"`
	} `json:"error"`
}

// classifyError maps an FCM error response onto a DeliveryState plus a stable
// reason string. The reason always carries the actionable code (errorCode if
// present, else the RPC status) so logs are explicit rather than opaque.
func classifyError(statusCode int, body []byte) (DeliveryState, string) {
	var er errorResponse
	_ = json.Unmarshal(body, &er)

	code := ""
	for _, d := range er.Error.Details {
		if strings.HasSuffix(d.Type, "FcmError") && d.ErrorCode != "" {
			code = d.ErrorCode
			break
		}
	}
	status := er.Error.Status
	msg := strings.TrimSpace(er.Error.Message)
	if msg == "" {
		msg = fmt.Sprintf("http %d", statusCode)
	}
	var reason string
	switch {
	case code != "":
		reason = code + ": " + msg
	case status != "":
		reason = status + ": " + msg
	default:
		reason = msg
	}

	switch code {
	case "UNREGISTERED", "INVALID_ARGUMENT":
		return StateNotRegistered, reason
	case "QUOTA_EXCEEDED", "TEMPORARY_BANNED":
		return StateBackoff, reason
	}
	// No specific FcmError code: fall back to HTTP semantics. A 5xx is
	// transient (retryable); any other non-2xx is a permanent failure.
	if statusCode >= 500 {
		return StateBackoff, reason
	}
	return StateFailed, reason
}

// nowUTC is a small helper kept for symmetry with the token source clock.
func nowUTC() time.Time { return time.Now().UTC() }
