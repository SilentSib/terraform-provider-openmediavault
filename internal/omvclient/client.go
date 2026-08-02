// Package omvclient implements a minimal client for the OpenMediaVault
// JSON-RPC API exposed at "<baseURL>/rpc.php".
//
// The OMV web UI (and this client) authenticate by POSTing a
// "session"/"login" RPC call and then reusing the resulting PHP session
// cookie for every subsequent call. There is no separate token/bearer
// auth mechanism in stock OMV.
//
// Every RPC call/response follows the same envelope:
//
//	Request:  {"service": "<Service>", "method": "<method>", "params": <any>}
//	Response: {"response": <any>, "error": null | {"code": int, "message": string, "trace": string}}
//
// NOTE: Service and method names (e.g. "ShareMgmt", "UserMgmt") were not
// independently verified against a live OMV 8 instance while scaffolding
// this provider. Confirm them against the target OMV version's engine
// RPC sources (usr/share/openmediavault/engined/rpc/*.inc) or via the
// browser network tab before relying on them in resources.
package omvclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds the connection settings for a Client.
type Config struct {
	// Host is the hostname or IP address of the OMV system, without scheme.
	Host string
	// Port is the TCP port the OMV web UI listens on (typically 80 or 443).
	Port int
	// Scheme is either "http" or "https".
	Scheme string
	// Username is the OMV account used to authenticate (e.g. "admin").
	Username string
	// Password is the OMV account password.
	Password string
	// InsecureSkipVerify disables TLS certificate verification. Useful for
	// the self-signed certificates OMV ships with by default, but should be
	// avoided in production where possible.
	InsecureSkipVerify bool
	// Timeout bounds every individual HTTP request made by the client.
	Timeout time.Duration
}

// Client is a small, session-authenticated JSON-RPC client for OMV.
type Client struct {
	cfg        Config
	httpClient *http.Client
	baseURL    string

	mu            sync.Mutex
	authenticated bool
}

// rpcRequest is the envelope every OMV RPC call must be wrapped in.
type rpcRequest struct {
	Service string      `json:"service"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

// rpcError mirrors the "error" object OMV returns on failure.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Trace   string `json:"trace,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("omv rpc error %d: %s", e.Code, e.Message)
}

// rpcResponse is the envelope every OMV RPC call returns.
type rpcResponse struct {
	Response json.RawMessage `json:"response"`
	Error    *rpcError       `json:"error"`
}

// New creates a Client from cfg. It performs no network I/O; call Login
// (or CheckMinVersion, which logs in implicitly) before issuing RPCs.
func New(cfg Config) (*Client, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("omvclient: host is required")
	}
	if cfg.Username == "" {
		return nil, fmt.Errorf("omvclient: username is required")
	}
	if cfg.Scheme == "" {
		cfg.Scheme = "https"
	}
	if cfg.Scheme != "http" && cfg.Scheme != "https" {
		return nil, fmt.Errorf("omvclient: scheme must be \"http\" or \"https\", got %q", cfg.Scheme)
	}
	if cfg.Port == 0 {
		if cfg.Scheme == "https" {
			cfg.Port = 443
		} else {
			cfg.Port = 80
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("omvclient: failed to create cookie jar: %w", err)
	}

	transport := &http.Transport{}
	if cfg.Scheme == "https" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify} // #nosec G402 -- opt-in via provider config
	}

	httpClient := &http.Client{
		Jar:       jar,
		Transport: transport,
		Timeout:   cfg.Timeout,
	}

	baseURL := fmt.Sprintf("%s://%s:%d/rpc.php", cfg.Scheme, cfg.Host, cfg.Port)

	return &Client{
		cfg:        cfg,
		httpClient: httpClient,
		baseURL:    baseURL,
	}, nil
}

// sessionLoginResponse mirrors Session::login()'s actual return shape (see
// var/www/openmediavault/rpc/session.inc in the OMV source), which is an
// object describing either a completed login or a pending MFA challenge --
// NOT a bare boolean. An earlier version of this client assumed a bool
// response (based on stale documentation, not the source), which fails to
// decode against a real OMV instance with a JSON error like "cannot
// unmarshal object into Go value of type bool".
type sessionLoginResponse struct {
	Username string `json:"username"`
	// Status is "authenticated" (login complete, session cookie now
	// valid) or "challengeRequired" (multi-factor auth configured for
	// this account; a second Session.verify call with a challenge
	// response is needed, which this client does not implement).
	Status    string            `json:"status"`
	SessionID string            `json:"sessionid,omitempty"`
	Challenge *sessionChallenge `json:"challenge,omitempty"`
}

type sessionChallenge struct {
	Kind        string `json:"kind"`
	RedirectURL string `json:"redirecturl"`
}

// Login authenticates against the "Session" RPC service. It must succeed
// before any other RPC call will be accepted by the server.
func (c *Client) Login(ctx context.Context) error {
	var resp sessionLoginResponse
	err := c.rawCall(ctx, "session", "login", map[string]string{
		"username": c.cfg.Username,
		"password": c.cfg.Password,
	}, &resp)
	if err != nil {
		// A wrong username/password surfaces as an RPC error here (HTTP
		// 400 "Incorrect username or password."), not as a "success" field
		// on the response -- the response struct above only ever
		// describes an already-successful (or challenge-pending) login.
		return fmt.Errorf("omvclient: login failed: %w", err)
	}

	switch resp.Status {
	case "authenticated":
		c.mu.Lock()
		c.authenticated = true
		c.mu.Unlock()
		return nil
	case "challengeRequired":
		kind := "unknown"
		if resp.Challenge != nil && resp.Challenge.Kind != "" {
			kind = resp.Challenge.Kind
		}
		return fmt.Errorf(
			"omvclient: login for user %q requires additional multi-factor authentication "+
				"(challenge kind %q), which this provider does not support. Use a dedicated "+
				"account with MFA disabled for Terraform automation, or disable MFA on this account",
			c.cfg.Username, kind,
		)
	default:
		return fmt.Errorf("omvclient: login returned an unrecognized status %q", resp.Status)
	}
}

// Logout terminates the current session. Safe to call even if not logged in.
func (c *Client) Logout(ctx context.Context) error {
	c.mu.Lock()
	authenticated := c.authenticated
	c.mu.Unlock()
	if !authenticated {
		return nil
	}
	if err := c.rawCall(ctx, "session", "logout", nil, nil); err != nil {
		return fmt.Errorf("omvclient: logout failed: %w", err)
	}
	c.mu.Lock()
	c.authenticated = false
	c.mu.Unlock()
	return nil
}

// Call invokes service.method with params and decodes the "response" field
// of the result into out (which should be a pointer, or nil to discard the
// response body). It transparently logs in first if the client does not yet
// hold a session.
func (c *Client) Call(ctx context.Context, service, method string, params interface{}, out interface{}) error {
	c.mu.Lock()
	authenticated := c.authenticated
	c.mu.Unlock()

	if !authenticated {
		if err := c.Login(ctx); err != nil {
			return err
		}
	}
	return c.rawCall(ctx, service, method, params, out)
}

// rawCall performs a single HTTP round trip without any auth bookkeeping.
func (c *Client) rawCall(ctx context.Context, service, method string, params interface{}, out interface{}) error {
	reqBody, err := json.Marshal(rpcRequest{
		Service: service,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return fmt.Errorf("omvclient: failed to encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("omvclient: failed to build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Requested-With", "XMLHttpRequest")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("omvclient: request to %s failed: %w", c.baseURL, err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("omvclient: failed to read response body: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("omvclient: unexpected HTTP status %s calling %s.%s: %s",
			httpResp.Status, service, method, strings.TrimSpace(string(body)))
	}

	var rpcResp rpcResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return fmt.Errorf("omvclient: failed to decode RPC envelope from %s.%s: %w (body: %s)",
			service, method, err, truncate(string(body), 500))
	}
	if rpcResp.Error != nil {
		return rpcResp.Error
	}

	if out == nil || len(rpcResp.Response) == 0 || string(rpcResp.Response) == "null" {
		return nil
	}
	if err := json.Unmarshal(rpcResp.Response, out); err != nil {
		return fmt.Errorf("omvclient: failed to decode response from %s.%s: %w", service, method, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// NewObjectUUID is the literal sentinel value OMV uses in the "uuid" field
// of a config object to mean "generate a new UUID for me". It comes from
// the OMV_CONFIGOBJECT_NEW_UUID environment variable, which is fixed at
// this value in every OMV installation (see
// debian/openmediavault.openmediavault.default in the OMV source), not a
// magic string like "new" or "newuuid".
const NewObjectUUID = "fa4b1c66-ef79-11e5-87a0-0002b3a176b4"

// ApplyChanges calls Config.applyChanges, which deploys the configuration
// for the given modules (or every dirty module, if modules is empty) --
// e.g. regenerating smb.conf/exports and restarting the relevant daemons.
// Until this is called (or done from the OMV web UI), changes made via
// other RPCs are only staged in the configuration database.
//
// IMPORTANT: the set of "dirty" modules is a single, instance-wide queue
// shared by every admin session and by the web UI -- it is NOT scoped to
// the objects a single caller just changed. Calling this with an empty
// modules list (as the OMV web UI's "Apply" button does) will deploy
// *every* pending change on the system, including ones made by other
// users/tools. Passing an explicit modules list limits the deploy step to
// those modules, but any other dirty modules remain pending regardless.
func (c *Client) ApplyChanges(ctx context.Context, modules []string, force bool) ([]string, error) {
	if modules == nil {
		modules = []string{}
	}
	var applied []string
	err := c.Call(ctx, "Config", "applyChanges", map[string]interface{}{
		"modules": modules,
		"force":   force,
	}, &applied)
	if err != nil {
		return nil, err
	}
	return applied, nil
}

// RevertChanges calls Config.revertChanges, which discards ALL pending
// (not-yet-applied) configuration database changes and clears the entire
// dirty-modules queue -- equivalent to clicking "Undo" in the OMV web UI's
// pending-changes bar. There is no way to scope this to a single object or
// module: it is instance-wide and will discard any other admin's or
// tool's unrelated pending changes too. Use with real caution; see
// OMVProvider's revert_on_apply_failure option for where this is wired
// into the provider, off by default.
func (c *Client) RevertChanges(ctx context.Context, filename string) error {
	return c.Call(ctx, "Config", "revertChanges", map[string]string{
		"filename": filename,
	}, nil)
}

// IsDirty calls Config.isDirty to check whether any (or, if modules is
// non-empty, any of the given) modules have pending changes not yet
// deployed via ApplyChanges.
func (c *Client) IsDirty(ctx context.Context, modules []string) (bool, error) {
	if modules == nil {
		modules = []string{}
	}
	var dirty bool
	err := c.Call(ctx, "Config", "isDirty", map[string]interface{}{
		"modules": modules,
	}, &dirty)
	if err != nil {
		return false, err
	}
	return dirty, nil
}

// SystemInformation is the subset of System.getInformation fields this
// provider currently cares about, verified against
// engined/rpc/system.inc in the OMV 8.5.5 source. OMV returns several
// other fields (cpuUtilization, memTotal, memFree, loadAverage, ...) that
// are intentionally NOT modeled here: system.inc's own doc comment warns
// "all numbers that might be > 4GiB are returned as strings to keep the
// 32bit compatibility", meaning several numeric-looking fields can come
// back as either a JSON number or a JSON string depending on the value at
// runtime -- decoding them into a fixed Go numeric type (as an earlier
// version of this file did for "memTotal") fails unpredictably depending
// on how much RAM the target system has. Add fields here only with a type
// that tolerates both encodings if a future resource needs them.
//
// The "version"/"cpuModelName"/"kernel" fields (along with everything
// else besides ts/time/hostname) are only present in the response at all
// when the authenticated account has the administrator role -- a
// non-admin account will get a response with an empty Version, which
// CheckMinVersion below reports as a distinct error rather than a
// confusing parse failure.
type SystemInformation struct {
	Hostname string `json:"hostname"`
	// Version is formatted as "<dpkg version> (<release codename>)", e.g.
	// "8.5.5-1 (Shaitung)" -- CheckMinVersion only looks at the leading
	// numeric component before the first ".", so the trailing codename
	// doesn't need to be stripped here.
	Version string `json:"version"`
}

// GetSystemInformation calls System.getInformation.
func (c *Client) GetSystemInformation(ctx context.Context) (*SystemInformation, error) {
	var info SystemInformation
	if err := c.Call(ctx, "System", "getInformation", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// CheckMinVersion logs in (if necessary), fetches the system version and
// returns an error if its major version is below min. OMV version strings
// look like "8.0.14-1 (CodeName)" (Debian package version style, plus a
// release name); only the leading numeric component before the first "."
// is treated as the major version.
func (c *Client) CheckMinVersion(ctx context.Context, min int) error {
	info, err := c.GetSystemInformation(ctx)
	if err != nil {
		return fmt.Errorf("omvclient: unable to determine OMV version: %w", err)
	}

	if info.Version == "" {
		return fmt.Errorf(
			"omvclient: System.getInformation did not return a version field; the account used " +
				"to authenticate must have the administrator role for OMV to include it",
		)
	}

	major, err := parseMajorVersion(info.Version)
	if err != nil {
		return fmt.Errorf("omvclient: unable to parse OMV version %q: %w", info.Version, err)
	}

	if major < min {
		return fmt.Errorf("omvclient: connected OMV instance reports version %q, but this provider requires OpenMediaVault %d or newer", info.Version, min)
	}
	return nil
}

func parseMajorVersion(version string) (int, error) {
	v := strings.TrimSpace(version)
	if v == "" {
		return 0, fmt.Errorf("empty version string")
	}
	// Take everything before the first "." as the major version.
	if idx := strings.IndexByte(v, '.'); idx >= 0 {
		v = v[:idx]
	}
	// Guard against unexpected leading non-numeric characters.
	v = strings.TrimFunc(v, func(r rune) bool {
		return r < '0' || r > '9'
	})
	return strconv.Atoi(v)
}
