package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

// ShutdownTarget is the connection info for a Proxmox VE or TrueNAS machine,
// built from its settings with Machine.apiTarget.
type ShutdownTarget struct {
	Name      string
	Type      string // "proxmox" or "truenas"
	Host      string // e.g. https://192.168.1.10:8006 or https://192.168.1.20
	Node      string // Proxmox node name
	Username  string // TrueNAS user that owns the API key
	Token     string // Proxmox: USER@REALM!TOKENID=SECRET, TrueNAS: API key
	VerifyTLS bool   // false allows the default self-signed certificates
}

const remoteTimeout = 20 * time.Second

// remoteShutdown asks the target to power off. A nil error means the target accepted the request.
func remoteShutdown(t ShutdownTarget, reason string) error {
	switch strings.ToLower(t.Type) {
	case "proxmox":
		if t.Node == "" {
			return fmt.Errorf("proxmox target %q has no node name", t.Name)
		}
		form := url.Values{"command": {"shutdown"}}
		_, err := proxmoxRequest(t, http.MethodPost, "/nodes/"+url.PathEscape(t.Node)+"/status", form)
		return err
	case "truenas":
		ws, err := truenasLogin(t)
		if err != nil {
			return err
		}
		defer ws.Close()
		if _, err := truenasCall(ws, 2, "system.shutdown", reason); err != nil {
			return fmt.Errorf("%w (the server may already be shutting down)", err)
		}
		return nil
	}
	return fmt.Errorf("unknown target type %q (use proxmox or truenas)", t.Type)
}

// checkRemoteTarget verifies the host and credentials without shutting anything down.
func checkRemoteTarget(t ShutdownTarget) (string, error) {
	switch strings.ToLower(t.Type) {
	case "proxmox":
		body, err := proxmoxRequest(t, http.MethodGet, "/version", nil)
		if err != nil {
			return "", err
		}
		var v struct {
			Data struct {
				Version string `json:"version"`
			} `json:"data"`
		}
		json.Unmarshal(body, &v)
		if t.Node == "" {
			return "", fmt.Errorf("authenticated (Proxmox %s) but no node name is set", v.Data.Version)
		}
		return "Proxmox VE " + v.Data.Version, nil
	case "truenas":
		ws, err := truenasLogin(t)
		if err != nil {
			return "", err
		}
		ws.Close()
		return "TrueNAS login OK", nil
	}
	return "", fmt.Errorf("unknown target type %q (use proxmox or truenas)", t.Type)
}

// normalizeHost returns scheme://host[:port], adding https:// and a default port when missing.
func normalizeHost(host, defaultPort string) (*url.URL, error) {
	host = strings.TrimSpace(host)
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, err := url.Parse(host)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid host %q", host)
	}
	if u.Port() == "" && defaultPort != "" {
		u.Host = net.JoinHostPort(u.Hostname(), defaultPort)
	}
	return u, nil
}

func tlsConfigFor(t ShutdownTarget) *tls.Config {
	return &tls.Config{InsecureSkipVerify: !t.VerifyTLS}
}

func proxmoxRequest(t ShutdownTarget, method, path string, form url.Values) ([]byte, error) {
	base, err := normalizeHost(t.Host, "8006")
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, base.Scheme+"://"+base.Host+"/api2/json"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+t.Token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	client := &http.Client{
		Timeout:   remoteTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsConfigFor(t)},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	detail := strings.TrimSpace(string(data))
	switch resp.StatusCode {
	case http.StatusOK:
		return data, nil
	case http.StatusUnauthorized:
		return nil, errors.New("Proxmox rejected the API token (401). Check it's USER@REALM!TOKENID=SECRET with the right realm " +
			"(root@pam, not root@pve), and that the token still exists under Datacenter > Permissions > API Tokens")
	case http.StatusForbidden:
		return nil, fmt.Errorf("Proxmox accepted the token but it isn't allowed to do this (403). "+
			"Give it Sys.PowerMgmt on /nodes/%s, or untick Privilege Separation on the token. %s", t.Node, detail)
	}
	if detail == "" {
		return nil, fmt.Errorf("proxmox API %s", resp.Status)
	}
	return nil, fmt.Errorf("proxmox API %s: %s", resp.Status, detail)
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

// truenasLogin opens the JSON-RPC WebSocket API (TrueNAS 25.04+, required on 26+) and authenticates.
func truenasLogin(t ShutdownTarget) (*websocket.Conn, error) {
	base, err := normalizeHost(t.Host, "")
	if err != nil {
		return nil, err
	}
	// TrueNAS revokes API keys that are sent over plain HTTP, so never allow it.
	if base.Scheme != "https" {
		return nil, fmt.Errorf("truenas host must use https (API keys sent over http are revoked)")
	}
	if t.Username == "" {
		return nil, fmt.Errorf("truenas target %q has no username (the user that owns the API key)", t.Name)
	}

	cfg, err := websocket.NewConfig("wss://"+base.Host+"/api/current", "https://"+base.Host)
	if err != nil {
		return nil, err
	}
	cfg.TlsConfig = tlsConfigFor(t)
	cfg.Dialer = &net.Dialer{Timeout: remoteTimeout}

	ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
	defer cancel()
	ws, err := cfg.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	ws.SetDeadline(time.Now().Add(remoteTimeout))

	result, err := truenasCall(ws, 1, "auth.login_ex", map[string]string{
		"mechanism": "API_KEY_PLAIN",
		"username":  t.Username,
		"api_key":   t.Token,
	})
	if err != nil {
		ws.Close()
		return nil, err
	}
	var login struct {
		ResponseType string `json:"response_type"`
	}
	json.Unmarshal(result, &login)
	if login.ResponseType != "SUCCESS" {
		ws.Close()
		return nil, fmt.Errorf("truenas login failed: %s", login.ResponseType)
	}
	return ws, nil
}

func truenasCall(ws *websocket.Conn, id int, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := websocket.JSON.Send(ws, req); err != nil {
		return nil, err
	}
	for {
		var resp rpcResponse
		if err := websocket.JSON.Receive(ws, &resp); err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		if resp.ID == nil || *resp.ID != id {
			continue // event notification or unrelated reply
		}
		if resp.Error != nil {
			detail := strings.TrimSpace(string(resp.Error.Data))
			if len(detail) > 300 {
				detail = detail[:300] + "..."
			}
			return nil, fmt.Errorf("%s: %s %s", method, resp.Error.Message, detail)
		}
		return resp.Result, nil
	}
}
