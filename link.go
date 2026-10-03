package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server <-> client link.
//
// The server reads NUT and decides when each client shuts down. Clients never listen on
// the network: each one dials out to the server over TLS, pins the server's certificate
// fingerprint, and authenticates with its own key. The only command a client acts on is
// "shutdown"; status messages are informational (shown in the UI and used for the failsafe).

const (
	defaultLinkPort   = 3494 // one above NUT's 3493
	linkHeartbeat     = 15 * time.Second
	linkReadTimeout   = 45 * time.Second
	linkMaxLine       = 4096
	linkRetryInterval = 10 * time.Second
)

type linkMsg struct {
	Type       string `json:"type"` // hello, welcome, error, status, shutdown, ack, shutdown_failed, ping
	ID         string `json:"id,omitempty"`
	Key        string `json:"key,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	Error      string `json:"error,omitempty"`
	Reason     string `json:"reason,omitempty"`
	UPS        string `json:"ups,omitempty"`
	Status     string `json:"status,omitempty"`
	Charge     int    `json:"charge,omitempty"`
	ChargeOK   bool   `json:"charge_known,omitempty"`
	ShutdownAt int    `json:"shutdown_at,omitempty"`
}

// systemShutdown is what a client runs when told to shut down (swapped out in tests).
var systemShutdown = shutdownSystem

func newClientKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------- TLS identity ----------

// loadOrCreateLinkCert returns the server's self-signed certificate and its SHA-256 fingerprint.
func loadOrCreateLinkCert() (tls.Certificate, string, error) {
	certPath := filepath.Join(appDir, "link-cert.pem")
	keyPath := filepath.Join(appDir, "link-key.pem")
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
		tmpl := &x509.Certificate{
			SerialNumber: serial,
			Subject:      pkix.Name{CommonName: "ups-monitor server"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().AddDate(30, 0, 0),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		keyDER, _ := x509.MarshalECPrivateKey(key)
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
			return tls.Certificate{}, "", err
		}
		if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
			return tls.Certificate{}, "", err
		}
		log.Println("[LINK] Created server identity (link-cert.pem). Existing pairing codes stay valid while this file is kept.")
		if cert, err = tls.LoadX509KeyPair(certPath, keyPath); err != nil {
			return tls.Certificate{}, "", err
		}
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return cert, hex.EncodeToString(sum[:]), nil
}

// ---------- Pairing codes ----------

// pairingCode is everything a client needs: upsmon://host:port?id=NAME&key=KEY&fp=FINGERPRINT
func pairingCode(hostPort, id, key, fingerprint string) string {
	q := url.Values{"id": {id}, "key": {key}, "fp": {fingerprint}}
	return "upsmon://" + hostPort + "?" + q.Encode()
}

func parsePairingCode(code string) (address, id, key, fp string, err error) {
	// Copying from a terminal can pick up line breaks where the code wrapped.
	u, err := url.Parse(strings.Join(strings.Fields(code), ""))
	if err != nil || u.Scheme != "upsmon" || u.Host == "" {
		return "", "", "", "", errors.New("not a pairing code (it should start with upsmon://)")
	}
	q := u.Query()
	id, key, fp = q.Get("id"), q.Get("key"), q.Get("fp")
	if id == "" || key == "" || fp == "" {
		return "", "", "", "", errors.New("pairing code is incomplete; copy it again from the server")
	}
	return u.Host, id, key, fp, nil
}

// lanAddresses lists this machine's non-loopback IPv4 addresses, for building pairing codes.
func lanAddresses() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
			out = append(out, ipn.IP.String())
		}
	}
	sort.Strings(out)
	return out
}

func withDefaultPort(addr string, port int) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(strings.Trim(addr, "[]"), strconv.Itoa(port))
}

// ---------- Wire helpers ----------

func writeLinkMsg(conn net.Conn, mu *sync.Mutex, m linkMsg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = conn.Write(append(data, '\n'))
	return err
}

func newLinkScanner(conn net.Conn) *bufio.Scanner {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, linkMaxLine), linkMaxLine)
	return sc
}

// ---------- Server side ----------

type clientConn struct {
	name  string
	key   string
	addr  string
	since time.Time
	conn  net.Conn
	wmu   sync.Mutex
}

func (c *clientConn) send(m linkMsg) error { return writeLinkMsg(c.conn, &c.wmu, m) }

// clientState tracks one client during an outage; it is reset when its UPS is back online.
type clientState struct {
	announced bool
	acked     bool
	failed    string
}

type linkHub struct {
	mu          sync.Mutex
	ln          net.Listener
	port        int
	lastErr     string
	cert        tls.Certificate
	fingerprint string
	conns       map[string]*clientConn
}

var (
	hub          = &linkHub{conns: make(map[string]*clientConn)}
	clientStates = make(map[string]*clientState) // guarded by stateMu
)

// identity loads (or creates) the server certificate once.
func (h *linkHub) identity() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fingerprint != "" {
		return h.fingerprint, nil
	}
	cert, fp, err := loadOrCreateLinkCert()
	if err != nil {
		return "", err
	}
	h.cert, h.fingerprint = cert, fp
	return fp, nil
}

// ensure keeps the client listener running on the configured port.
func (h *linkHub) ensure(store *settingsStore, port int) {
	if _, err := h.identity(); err != nil {
		h.logErrOnce(fmt.Sprintf("[LINK] Cannot create server certificate: %v", err))
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ln != nil && h.port == port {
		return
	}
	if h.ln != nil {
		h.ln.Close()
		h.ln = nil
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		h.logErrOnceLocked(fmt.Sprintf("[LINK] Cannot listen for clients on port %d: %v", port, err))
		return
	}
	h.ln, h.port, h.lastErr = ln, port, ""
	log.Printf("[LINK] Listening for clients on port %d.", port)
	go h.acceptLoop(store, ln)
}

func (h *linkHub) logErrOnce(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logErrOnceLocked(msg)
}

func (h *linkHub) logErrOnceLocked(msg string) {
	if msg != h.lastErr {
		log.Println(msg)
		h.lastErr = msg
	}
}

// stop closes the listener and every client connection (used when leaving server mode).
func (h *linkHub) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ln == nil {
		return
	}
	h.ln.Close()
	h.ln = nil
	for _, c := range h.conns {
		c.conn.Close()
	}
	log.Println("[LINK] Stopped listening for clients.")
}

func (h *linkHub) get(name string) *clientConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[name]
}

// prune disconnects clients that were removed or whose key changed.
func (h *linkHub) prune(machines []Machine) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, c := range h.conns {
		ok := false
		for _, m := range machines {
			if m.Type == MachineClient && m.Name == name && m.ClientKey == c.key {
				ok = true
			}
		}
		if !ok {
			log.Printf("[LINK] Disconnecting %s: removed or key changed.", name)
			c.conn.Close()
			delete(h.conns, name)
		}
	}
}

func (h *linkHub) acceptLoop(store *settingsStore, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go h.serve(store, conn)
	}
}

func (h *linkHub) serve(store *settingsStore, raw net.Conn) {
	h.mu.Lock()
	cert := h.cert
	h.mu.Unlock()
	conn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := conn.Handshake(); err != nil {
		return
	}

	sc := newLinkScanner(conn)
	var hello linkMsg
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &hello) != nil || hello.Type != "hello" {
		return
	}
	target, srv, found := linkedClient(store, hello.ID)
	remote := raw.RemoteAddr().String()
	if !found || subtle.ConstantTimeCompare([]byte(target.ClientKey), []byte(hello.Key)) != 1 {
		log.Printf("[LINK] Rejected connection from %s (client %q): unknown client or wrong key.", remote, hello.ID)
		time.Sleep(time.Second) // slow down guessing
		var mu sync.Mutex
		writeLinkMsg(conn, &mu, linkMsg{Type: "error", Error: "unknown client or wrong key"})
		return
	}

	webhook := srv.webhookFor(target)
	cc := &clientConn{name: target.Name, key: target.ClientKey, addr: remote, since: time.Now(), conn: conn}
	h.mu.Lock()
	if old := h.conns[cc.name]; old != nil {
		old.conn.Close()
	}
	h.conns[cc.name] = cc
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.conns[cc.name] == cc {
			delete(h.conns, cc.name)
		}
		h.mu.Unlock()
		log.Printf("[LINK] Client %s disconnected.", cc.name)
	}()

	conn.SetDeadline(time.Time{})
	if cc.send(linkMsg{Type: "welcome"}) != nil {
		return
	}
	log.Printf("[LINK] Client %s connected from %s (%s).", cc.name, remote, hello.Hostname)

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(linkHeartbeat)
		defer ticker.Stop()
		for {
			sendClientStatus(store, cc)
			select {
			case <-ticker.C:
			case <-done:
				return
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(linkReadTimeout))
		if !sc.Scan() {
			return
		}
		var m linkMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "ack":
			stateMu.Lock()
			if s := clientStates[cc.name]; s != nil && !s.acked {
				s.acked, s.failed = true, ""
				msg := fmt.Sprintf("🔌 **Client Shutdown**: `%s` confirmed it is shutting down.", cc.name)
				log.Println(msg)
				sendDiscordWebhookAsync(webhook, msg)
			}
			stateMu.Unlock()
		case "shutdown_failed":
			stateMu.Lock()
			if s := clientStates[cc.name]; s != nil {
				s.acked, s.failed = false, m.Error
			}
			stateMu.Unlock()
			msg := fmt.Sprintf("❌ **Client Shutdown Failed**: `%s`: %s. Retrying.", cc.name, m.Error)
			log.Println(msg)
			sendDiscordWebhookAsync(webhook, msg)
		}
	}
}

// linkedClient finds a client machine in the live settings.
func linkedClient(store *settingsStore, name string) (Machine, *ServerSettings, bool) {
	s := store.Get()
	if !s.isServer() {
		return Machine{}, nil, false
	}
	for _, m := range s.Server.Machines {
		if m.Type == MachineClient && m.Name == name {
			return m, s.Server, true
		}
	}
	return Machine{}, nil, false
}

// sendClientStatus tells a client the current state of its UPS (information only).
func sendClientStatus(store *settingsStore, cc *clientConn) {
	target, _, ok := linkedClient(store, cc.name)
	if !ok {
		return
	}
	stateMu.Lock()
	r, seen := upsReadings[target.UPS]
	stateMu.Unlock()
	m := linkMsg{Type: "status", UPS: target.UPS, ShutdownAt: target.ShutdownAt}
	if seen {
		m.Status, m.Charge, m.ChargeOK = r.Status, r.Charge, r.ChargeKnown
	}
	cc.send(m)
}

// handleClientShutdowns tells each linked client to shut down once its UPS reaches its limit.
// The order is repeated every poll until the client confirms.
func handleClientShutdowns(srv *ServerSettings, e upsEvent) {
	stateMu.Lock()
	defer stateMu.Unlock()

	for _, c := range srv.Machines {
		if c.Type != MachineClient || c.UPS != e.ups {
			continue
		}
		if !e.onBattery {
			if s := clientStates[c.Name]; s != nil && s.acked {
				log.Printf("[INFO] %s is back online; re-arming shutdown for client %s.", e.ups, c.Name)
			}
			delete(clientStates, c.Name)
			continue
		}
		if !e.reachedShutdown(c.ShutdownAt) {
			continue
		}
		s := clientStates[c.Name]
		if s == nil {
			s = &clientState{}
			clientStates[c.Name] = s
		}
		if s.acked {
			continue
		}
		cc := hub.get(c.Name)
		if !s.announced {
			s.announced = true
			msg := fmt.Sprintf("⚠️ **UPS Shutdown**: Battery at %d%%. Telling client `%s` to shut down.", e.charge, c.Name)
			log.Println(msg)
			sendDiscordWebhookAsync(srv.webhookFor(c), msg)
			if cc == nil {
				log.Printf("[LINK] Client %s is not connected; it will be told to shut down if it reconnects.", c.Name)
			}
		}
		if cc != nil {
			reason := fmt.Sprintf("UPS %s on battery (%s), charge %d%%", e.ups, e.status, e.charge)
			go cc.send(linkMsg{Type: "shutdown", Reason: reason, Charge: e.charge})
		}
	}
}

// clientLinkState describes a linked client for the server's status screen.
func clientLinkState(name string) string {
	if s := clientStates[name]; s != nil {
		switch {
		case s.acked:
			return "shutdown confirmed"
		case s.failed != "":
			return "shutdown failed: " + s.failed
		case s.announced:
			return "shutdown ordered, waiting for client"
		}
	}
	if c := hub.get(name); c != nil {
		return "connected from " + c.addr
	}
	return "not connected"
}

// ---------- Client side ----------

type clientLink struct {
	connected     bool
	everConnected bool
	err           string
	lostAt        time.Time
	lastOnBattery bool
	shutdownAt    int
	upsName       string
	shutdownDone  bool
}

var link clientLink // guarded by stateMu

func setLinkConnected(connected bool, err error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if connected && !link.connected {
		log.Println("[LINK] Connected to the UPS monitor server.")
		link.everConnected = true
	}
	if !connected && link.connected {
		log.Printf("[LINK] Lost connection to the server: %v", err)
		link.lostAt = time.Now()
	}
	if !connected && !link.connected && err != nil && err.Error() != link.err {
		log.Printf("[LINK] Cannot connect to the server: %v", err)
	}
	link.connected = connected
	link.err = ""
	if err != nil {
		link.err = err.Error()
	}
}

func dialServer(conf ClientSettings) (net.Conn, error) {
	want := strings.ToLower(strings.ReplaceAll(conf.ServerFingerprint, ":", ""))
	tlsConf := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The server uses a self-signed certificate; trust comes from the pinned fingerprint instead of a CA.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("server sent no certificate")
			}
			sum := sha256.Sum256(raw[0])
			if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(want)) != 1 {
				return errors.New("server certificate does not match the paired fingerprint (re-pair if the server was reinstalled)")
			}
			return nil
		},
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", withDefaultPort(conf.ServerAddress, defaultLinkPort), tlsConf)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	var mu sync.Mutex
	if err := writeLinkMsg(conn, &mu, linkMsg{Type: "hello", ID: conf.Name, Key: conf.Key, Hostname: host}); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	sc := newLinkScanner(conn)
	var reply linkMsg
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &reply) != nil {
		conn.Close()
		return nil, errors.New("no reply from server")
	}
	if reply.Type != "welcome" {
		conn.Close()
		return nil, fmt.Errorf("server refused: %s", reply.Error)
	}
	return &scannedConn{Conn: conn, sc: sc}, nil
}

// scannedConn keeps the scanner that already buffered data past the welcome line.
type scannedConn struct {
	net.Conn
	sc *bufio.Scanner
}

// runClientLink holds one connection to the server and returns when it drops or settings change.
func runClientLink(store *settingsStore, conf ClientSettings) {
	clientFailsafe(conf)
	conn, err := dialServer(conf)
	if err != nil {
		setLinkConnected(false, err)
		store.wait(linkRetryInterval)
		return
	}
	setLinkConnected(true, nil)

	sconn := conn.(*scannedConn)
	var wmu sync.Mutex
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(linkHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeLinkMsg(conn, &wmu, linkMsg{Type: "ping"})
				store.ReloadIfChanged() // a hand edit wakes the store.changed case below
			case <-store.changed:
				conn.Close() // reconnect with the new settings
				return
			case <-done:
				return
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(linkReadTimeout))
		if !sconn.sc.Scan() {
			err := sconn.sc.Err()
			if err == nil {
				err = errors.New("server closed the connection")
			}
			conn.Close()
			setLinkConnected(false, err)
			return
		}
		var m linkMsg
		if json.Unmarshal(sconn.sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "status":
			stateMu.Lock()
			upsReadings[m.UPS] = upsReading{Status: m.Status, Charge: m.Charge, ChargeKnown: m.ChargeOK, Seen: time.Now()}
			link.upsName, link.shutdownAt = m.UPS, m.ShutdownAt
			link.lastOnBattery = isOnBatteryStatus(m.Status)
			if !link.lastOnBattery {
				link.shutdownDone = false
			}
			stateMu.Unlock()
		case "shutdown":
			if err := clientShutdown(conf, fmt.Sprintf("⚠️ **UPS Shutdown**: Server ordered shutdown (%s). Shutting down system now.", m.Reason)); err != nil {
				writeLinkMsg(conn, &wmu, linkMsg{Type: "shutdown_failed", Error: err.Error()})
				continue
			}
			writeLinkMsg(conn, &wmu, linkMsg{Type: "ack"})
		default:
			// Clients only ever act on "shutdown"; anything else is ignored.
		}
	}
}

// clientShutdown runs the local shutdown once per outage.
func clientShutdown(conf ClientSettings, msg string) error {
	stateMu.Lock()
	already := link.shutdownDone
	stateMu.Unlock()
	if already {
		return nil
	}
	log.Println(msg)
	sendDiscordWebhook(conf.WebhookURL, msg)
	if err := systemShutdown(); err != nil {
		log.Printf("[ERROR] Shutdown command failed: %v", err)
		sendDiscordWebhook(conf.WebhookURL, fmt.Sprintf("❌ **Shutdown Failed**: %v.", err))
		return err
	}
	stateMu.Lock()
	link.shutdownDone = true
	stateMu.Unlock()
	return nil
}

// clientFailsafe shuts down if the server went silent while the UPS was on battery,
// e.g. the network switch lost power. It only uses the last status the server sent.
func clientFailsafe(conf ClientSettings) {
	if conf.FailsafeSeconds <= 0 {
		return
	}
	stateMu.Lock()
	trigger := !link.connected && link.lastOnBattery && !link.lostAt.IsZero() &&
		time.Since(link.lostAt) >= time.Duration(conf.FailsafeSeconds)*time.Second
	stateMu.Unlock()
	if trigger {
		clientShutdown(conf, fmt.Sprintf("⚠️ **UPS Shutdown**: Lost contact with the server for %ds while on battery. Shutting down system now.", conf.FailsafeSeconds))
	}
}
