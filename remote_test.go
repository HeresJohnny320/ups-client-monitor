package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

func TestProxmoxShutdown(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.Method + " " + r.URL.Path
		r.ParseForm()
		gotBody = r.PostForm.Get("command")
		w.Write([]byte(`{"data":null}`))
	}))
	defer srv.Close()

	target := ShutdownTarget{Name: "pve", Type: "proxmox", Host: srv.URL, Node: "pve1", Token: "root@pam!ups=secret"}
	if err := remoteShutdown(target, "test"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "PVEAPIToken=root@pam!ups=secret" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotPath != "POST /api2/json/nodes/pve1/status" || gotBody != "shutdown" {
		t.Errorf("request = %q command=%q", gotPath, gotBody)
	}

	target.VerifyTLS = true
	if err := remoteShutdown(target, "test"); err == nil {
		t.Error("expected TLS verification failure against a self-signed cert")
	}
}

func TestProxmoxPermissionError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"data":null}`, http.StatusForbidden)
	}))
	defer srv.Close()

	err := remoteShutdown(ShutdownTarget{Type: "proxmox", Host: srv.URL, Node: "pve1", Token: "x"}, "test")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 error, got %v", err)
	}
}

func fakeTrueNAS(t *testing.T, apiKey string, calls *[]string) *httptest.Server {
	return httptest.NewTLSServer(websocket.Handler(func(ws *websocket.Conn) {
		if ws.Request().URL.Path != "/api/current" {
			t.Errorf("path = %s", ws.Request().URL.Path)
			return
		}
		for {
			var req struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
				Params []any  `json:"params"`
			}
			if err := websocket.JSON.Receive(ws, &req); err != nil {
				return
			}
			*calls = append(*calls, req.Method)
			// Unrelated event notifications must be skipped by the client.
			websocket.JSON.Send(ws, map[string]any{"jsonrpc": "2.0", "method": "collection_update"})
			switch req.Method {
			case "auth.login_ex":
				creds := req.Params[0].(map[string]any)
				resp := "AUTH_ERR"
				if creds["mechanism"] == "API_KEY_PLAIN" && creds["api_key"] == apiKey && creds["username"] == "truenas_admin" {
					resp = "SUCCESS"
				}
				websocket.JSON.Send(ws, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"response_type": resp}})
			case "system.shutdown":
				if len(req.Params) != 1 || req.Params[0] == "" {
					websocket.JSON.Send(ws, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "reason required"}})
					continue
				}
				websocket.JSON.Send(ws, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": 42})
			}
		}
	}))
}

func TestTrueNASShutdown(t *testing.T) {
	var calls []string
	srv := fakeTrueNAS(t, "good-key", &calls)
	defer srv.Close()

	target := ShutdownTarget{Name: "nas", Type: "truenas", Host: srv.URL, Username: "truenas_admin", Token: "good-key"}
	if err := remoteShutdown(target, "UPS on battery"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "auth.login_ex,system.shutdown" {
		t.Errorf("calls = %v", calls)
	}

	calls = nil
	target.Token = "bad-key"
	if err := remoteShutdown(target, "UPS on battery"); err == nil || len(calls) != 1 {
		t.Errorf("expected login failure and no shutdown call, got err=%v calls=%v", err, calls)
	}
}

func TestTrueNASRejectsHTTP(t *testing.T) {
	target := ShutdownTarget{Type: "truenas", Host: "http://192.0.2.1", Username: "truenas_admin", Token: "k"}
	if _, err := checkRemoteTarget(target); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected https error, got %v", err)
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"192.168.1.10":             "https://192.168.1.10:8006",
		"https://pve.lan":          "https://pve.lan:8006",
		"https://pve.lan:443/":     "https://pve.lan:443",
		"http://192.168.1.10:8006": "http://192.168.1.10:8006",
	}
	for in, want := range cases {
		u, err := normalizeHost(in, "8006")
		if err != nil || u.Scheme+"://"+u.Host != want {
			t.Errorf("normalizeHost(%q) = %v, %v; want %s", in, u, err, want)
		}
	}
}

func TestParseCharge(t *testing.T) {
	if c, ok := parseCharge("95.0"); !ok || c != 95 {
		t.Errorf("95.0 -> %d %v", c, ok)
	}
	if _, ok := parseCharge(""); ok {
		t.Error("empty charge should be unknown")
	}
}
