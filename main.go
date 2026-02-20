package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"os/signal"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

var copyBufPool = sync.Pool{New: func() any {
	// 64KiB tends to be a good compromise for tunneling.
	b := make([]byte, 64*1024)
	return &b
}}

func getCopyBuf() []byte {
	return *copyBufPool.Get().(*[]byte)
}

func putCopyBuf(b []byte) {
	// Avoid keeping huge buffers if something replaced it.
	if cap(b) < 64*1024 {
		return
	}
	b = b[:64*1024]
	copyBufPool.Put(&b)
}

func fastCopy(dst io.Writer, src io.Reader) {
	// Prefer zero-copy paths when available (net.TCPConn implements these).
	if wt, ok := src.(io.WriterTo); ok {
		_, _ = wt.WriteTo(dst)
		return
	}
	if rf, ok := dst.(io.ReaderFrom); ok {
		_, _ = rf.ReadFrom(src)
		return
	}
	buf := getCopyBuf()
	_, _ = io.CopyBuffer(dst, src, buf)
	putCopyBuf(buf)
}

const (
	defaultHTTPAddr = ":9876"
	defaultDataDir  = "/config"
	configFileName  = "porter.json"
)

//go:embed web/*
var embeddedWeb embed.FS

type Rule struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Protocol        string `json:"protocol"`
	ListenHost      string `json:"listen_host"`
	ListenPort      int    `json:"listen_port"`
	TargetType      string `json:"target_type"`
	TargetHost      string `json:"target_host,omitempty"`
	TargetContainer string `json:"target_container,omitempty"`
	TargetNetwork   string `json:"target_network,omitempty"`
	TargetPort      int    `json:"target_port"`
	Enabled         bool   `json:"enabled"`
}

type Config struct {
	Rules []Rule `json:"rules"`
}

// FileConfig is the on-disk config format.
// Keep API token here (not exposed via /api/config).
type FileConfig struct {
	APIToken string `json:"api_token,omitempty"`
	Rules    []Rule `json:"rules"`
}

type Manager struct {
	mu       sync.Mutex
	applyMu  sync.Mutex
	cfg      Config
	runners  map[string]Runner
	resolver *DockerResolver
}

func NewManager(resolver *DockerResolver) *Manager {
	return &Manager{
		cfg:      Config{Rules: []Rule{}},
		runners:  map[string]Runner{},
		resolver: resolver,
	}
}

func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := make([]Rule, len(m.cfg.Rules))
	copy(copied, m.cfg.Rules)
	return Config{Rules: copied}
}

func (m *Manager) ApplyConfig(cfg Config) error {
	// Serialize ApplyConfig calls to avoid concurrent map iteration/writes.
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	// ApplyConfig is designed to avoid global restarts. It computes which runners
	// need to stop/start based on rule changes and enabled state.
	m.mu.Lock()
	prevCfg := m.cfg
	prevRunners := make(map[string]Runner, len(m.runners))
	for k, v := range m.runners {
		prevRunners[k] = v
	}
	// Keep existing runners map; we will mutate it incrementally.
	if m.runners == nil {
		m.runners = map[string]Runner{}
	}
	m.cfg = cfg
	m.mu.Unlock()

	prevByID := map[string]Rule{}
	for _, r := range prevCfg.Rules {
		prevByID[r.ID] = r
	}

	nextByID := map[string]Rule{}
	for _, r := range cfg.Rules {
		nextByID[r.ID] = r
	}

	toStop := map[string]Runner{}
	toStart := []Rule{}

	// Decide which existing runners to stop.
	for id, runner := range prevRunners {
		prevRule, prevOk := prevByID[id]
		nextRule, nextOk := nextByID[id]
		switch {
		case !nextOk:
			toStop[id] = runner
		case !nextRule.Enabled:
			toStop[id] = runner
		case prevOk && !rulesEqualForRunner(prevRule, nextRule):
			toStop[id] = runner
		}
	}

	// Decide which rules need a runner started.
	for _, rule := range cfg.Rules {
		if !rule.Enabled {
			continue
		}
		prevRule, hadPrev := prevByID[rule.ID]
		_, hadRunner := prevRunners[rule.ID]
		needsStart := !hadRunner || (hadPrev && !rulesEqualForRunner(prevRule, rule))
		if needsStart {
			toStart = append(toStart, rule)
		}
	}

	// Stop runners (remove from live map before stopping).
	for id, runner := range toStop {
		m.mu.Lock()
		if cur, ok := m.runners[id]; ok && cur == runner {
			delete(m.runners, id)
		}
		m.mu.Unlock()
		_ = runner.Stop()
	}

	var errs []string
	for _, rule := range toStart {
		if err := m.startRule(rule); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", rule.ID, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (m *Manager) UpsertRule(rule Rule) (Config, error) {
	m.mu.Lock()
	rules := append([]Rule(nil), m.cfg.Rules...)
	m.mu.Unlock()

	cfg := Config{Rules: rules}

	updated := false
	for i := range cfg.Rules {
		if cfg.Rules[i].ID == rule.ID {
			cfg.Rules[i] = rule
			updated = true
			break
		}
	}

	if !updated {
		cfg.Rules = append(cfg.Rules, rule)
	}

	if err := m.ApplyConfig(cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func (m *Manager) DeleteRule(id string) (Config, error) {
	m.mu.Lock()
	rules := append([]Rule(nil), m.cfg.Rules...)
	m.mu.Unlock()

	cfg := Config{Rules: rules}

	filtered := make([]Rule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		if rule.ID == id {
			continue
		}
		filtered = append(filtered, rule)
	}
	cfg.Rules = filtered

	if err := m.ApplyConfig(cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func (m *Manager) startEnabled(cfg Config) error {
	var errs []string
	for _, rule := range cfg.Rules {
		if !rule.Enabled {
			continue
		}
		if err := m.startRule(rule); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", rule.ID, err))
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (m *Manager) startRule(rule Rule) error {
	listenAddr := net.JoinHostPort(rule.ListenHost, strconv.Itoa(rule.ListenPort))
	resolve := func() (string, error) {
		return m.resolveTarget(rule)
	}

	var runner Runner
	var err error

	switch rule.Protocol {
	case "tcp":
		runner, err = newTCPRunner(listenAddr, resolve)
	case "udp":
		runner, err = newUDPRunner(listenAddr, resolve)
	default:
		err = fmt.Errorf("unsupported protocol %q", rule.Protocol)
	}

	if err != nil {
		return err
	}

	m.mu.Lock()
	m.runners[rule.ID] = runner
	m.mu.Unlock()

	return nil
}

func (m *Manager) resolveTarget(rule Rule) (string, error) {
	if rule.TargetPort <= 0 || rule.TargetPort > 65535 {
		return "", fmt.Errorf("invalid target port %d", rule.TargetPort)
	}

	switch rule.TargetType {
	case "container":
		if rule.TargetContainer == "" {
			return "", errors.New("target container is required")
		}
		if m.resolver == nil {
			return "", errors.New("docker resolver unavailable")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		ip, err := m.resolver.ResolveContainerIP(ctx, rule.TargetContainer, rule.TargetNetwork)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(ip, strconv.Itoa(rule.TargetPort)), nil
	case "host":
		if rule.TargetHost == "" {
			return "", errors.New("target host is required")
		}
		return net.JoinHostPort(rule.TargetHost, strconv.Itoa(rule.TargetPort)), nil
	default:
		return "", fmt.Errorf("unsupported target type %q", rule.TargetType)
	}
}

type Runner interface {
	Stop() error
}

type tcpRunner struct {
	ln      net.Listener
	stop    chan struct{}
	wg      sync.WaitGroup
	resolve func() (string, error)
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
}

func newTCPRunner(addr string, resolve func() (string, error)) (*tcpRunner, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	runner := &tcpRunner{
		ln:      ln,
		stop:    make(chan struct{}),
		resolve: resolve,
		conns:   map[net.Conn]struct{}{},
	}

	runner.wg.Add(1)
	go runner.serve()

	return runner, nil
}

func (r *tcpRunner) serve() {
	defer r.wg.Done()

	for {
		conn, err := r.ln.Accept()
		if err != nil {
			select {
			case <-r.stop:
				return
			default:
				log.Printf("tcp accept error: %v", err)
				continue
			}
		}

		r.wg.Add(1)
		go func(c net.Conn) {
			defer r.wg.Done()
			r.mu.Lock()
			r.conns[c] = struct{}{}
			r.mu.Unlock()
			defer func() {
				r.mu.Lock()
				delete(r.conns, c)
				r.mu.Unlock()
				_ = c.Close()
			}()

			target, err := r.resolve()
			if err != nil {
				log.Printf("tcp resolve error: %v", err)
				return
			}

			upstream, err := net.Dial("tcp", target)
			if err != nil {
				log.Printf("tcp dial error: %v", err)
				return
			}
			defer upstream.Close()

			// Tunnel with pooled buffers to reduce per-connection allocations.
			// When one direction finishes, force the other to unblock.
			copyDone := make(chan struct{}, 2)
			go func() {
				fastCopy(upstream, c)
				copyDone <- struct{}{}
			}()
			go func() {
				fastCopy(c, upstream)
				copyDone <- struct{}{}
			}()
			<-copyDone
			_ = setConnDeadlineNow(c)
			_ = setConnDeadlineNow(upstream)
			<-copyDone
		}(conn)
	}
}

func (r *tcpRunner) Stop() error {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}

	err := r.ln.Close()

	r.mu.Lock()
	for c := range r.conns {
		_ = c.Close()
	}
	r.mu.Unlock()

	r.wg.Wait()
	return err
}

type udpRunner struct {
	conn         *net.UDPConn
	stop         chan struct{}
	wg           sync.WaitGroup
	resolve      func() (string, error)
	mu           sync.Mutex
	clients      map[string]*udpClient
	idleTimeout  time.Duration
	cleanupEvery time.Duration

	// target cache to reduce per-packet ResolveUDPAddr overhead.
	targetMu          sync.Mutex
	cachedTarget      string
	cachedTargetAddr  *net.UDPAddr
	cachedTargetAt    time.Time
	targetCacheMaxAge time.Duration
}

type udpClient struct {
	conn     *net.UDPConn
	lastSeen time.Time
}

func newUDPRunner(addr string, resolve func() (string, error)) (*udpRunner, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}

	runner := &udpRunner{
		conn:              conn,
		stop:              make(chan struct{}),
		resolve:           resolve,
		clients:           map[string]*udpClient{},
		idleTimeout:       2 * time.Minute,
		cleanupEvery:      30 * time.Second,
		targetCacheMaxAge: 2 * time.Second,
	}

	runner.wg.Add(2)
	go runner.serve()
	go runner.cleanup()

	return runner, nil
}

func (r *udpRunner) serve() {
	defer r.wg.Done()
	buf := getCopyBuf()
	defer putCopyBuf(buf)

	for {
		_ = r.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, addr, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				select {
				case <-r.stop:
					return
				default:
					continue
				}
			}
			select {
			case <-r.stop:
				return
			default:
				log.Printf("udp read error: %v", err)
				continue
			}
		}

		target, err := r.resolve()
		if err != nil {
			log.Printf("udp resolve error: %v", err)
			continue
		}

		targetAddr, err := r.resolveTargetAddr(target)
		if err != nil {
			log.Printf("udp target error: %v", err)
			continue
		}

		clientKey := addr.String() + "|" + targetAddr.String()
		client := r.getClient(clientKey, targetAddr, addr)
		if client == nil || client.conn == nil {
			continue
		}
		r.mu.Lock()
		client.lastSeen = time.Now()
		r.mu.Unlock()

		if _, err := client.conn.Write(buf[:n]); err != nil {
			log.Printf("udp write error: %v", err)
		}
	}
}

func (r *udpRunner) resolveTargetAddr(target string) (*net.UDPAddr, error) {
	// Target changes are expected (e.g., container IP). Cache briefly to avoid
	// resolving on every single packet.
	r.targetMu.Lock()
	if r.cachedTarget == target && r.cachedTargetAddr != nil && time.Since(r.cachedTargetAt) <= r.targetCacheMaxAge {
		addr := r.cachedTargetAddr
		r.targetMu.Unlock()
		return addr, nil
	}
	r.targetMu.Unlock()

	addr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}

	r.targetMu.Lock()
	r.cachedTarget = target
	r.cachedTargetAddr = addr
	r.cachedTargetAt = time.Now()
	r.targetMu.Unlock()
	return addr, nil
}

func (r *udpRunner) getClient(key string, targetAddr *net.UDPAddr, clientAddr *net.UDPAddr) *udpClient {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.clients[key]; ok {
		return existing
	}

	upstream, err := net.DialUDP("udp", nil, targetAddr)
	if err != nil {
		log.Printf("udp dial error: %v", err)
		return nil
	}

	client := &udpClient{conn: upstream, lastSeen: time.Now()}
	r.clients[key] = client

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		buf := getCopyBuf()
		defer putCopyBuf(buf)
		for {
			_ = upstream.SetReadDeadline(time.Now().Add(1 * time.Second))
			n, err := upstream.Read(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					select {
					case <-r.stop:
						return
					default:
						continue
					}
				}
				return
			}
			_, _ = r.conn.WriteToUDP(buf[:n], clientAddr)
		}
	}()

	return client
}

func (r *udpRunner) cleanup() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cleanupEvery)
	defer ticker.Stop()

	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-r.idleTimeout)
			r.mu.Lock()
			for key, client := range r.clients {
				if client.lastSeen.Before(cutoff) {
					_ = client.conn.Close()
					delete(r.clients, key)
				}
			}
			r.mu.Unlock()
		}
	}
}

func (r *udpRunner) Stop() error {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}

	err := r.conn.Close()

	r.mu.Lock()
	for _, client := range r.clients {
		if client.conn != nil {
			_ = client.conn.Close()
		}
	}
	r.clients = map[string]*udpClient{}
	r.mu.Unlock()

	r.wg.Wait()
	return err
}

type DockerResolver struct {
	cli *client.Client
}

func NewDockerResolver() (*DockerResolver, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &DockerResolver{cli: cli}, nil
}

func (d *DockerResolver) ResolveContainerIP(ctx context.Context, name string, network string) (string, error) {
	inspect, err := d.inspectContainer(ctx, name)
	if err != nil {
		return "", err
	}

	if inspect.NetworkSettings == nil {
		return "", errors.New("container has no network settings")
	}

	networks := inspect.NetworkSettings.Networks
	if network != "" {
		if info, ok := networks[network]; ok && info.IPAddress != "" {
			return info.IPAddress, nil
		}
		return "", fmt.Errorf("container not connected to network %q", network)
	}

	for _, info := range networks {
		if info.IPAddress != "" {
			return info.IPAddress, nil
		}
	}

	return "", errors.New("container has no IP on connected networks")
}

func (d *DockerResolver) inspectContainer(ctx context.Context, name string) (types.ContainerJSON, error) {
	inspect, err := d.cli.ContainerInspect(ctx, name)
	if err == nil {
		return inspect, nil
	}

	containers, listErr := d.cli.ContainerList(ctx, container.ListOptions{All: true})
	if listErr != nil {
		return types.ContainerJSON{}, listErr
	}

	for _, c := range containers {
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == name {
				return d.cli.ContainerInspect(ctx, c.ID)
			}
		}
		if c.Labels["com.docker.compose.service"] == name {
			return d.cli.ContainerInspect(ctx, c.ID)
		}
	}

	return types.ContainerJSON{}, err
}

type APIServer struct {
	configPath string
	manager    *Manager
	files      http.Handler
	apiToken   string
	envToken   bool
	tokenMu    sync.RWMutex
}

func NewAPIServer(configPath string, manager *Manager, apiToken string, envToken bool) *APIServer {
	webRoot, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		// Fail closed with a clear message instead of silently serving nothing.
		log.Printf("embedded web unavailable: %v", err)
		webRoot = os.DirFS(".")
	}
	return &APIServer{
		configPath: configPath,
		manager:    manager,
		files:      http.FileServer(http.FS(webRoot)),
		apiToken:   apiToken,
		envToken:   envToken,
	}
}

func (s *APIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	applySecurityHeaders(w)
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.handleAPI(w, r)
		return
	}
	s.files.ServeHTTP(w, r)
}

func (s *APIServer) authorize(r *http.Request) bool {
	s.tokenMu.RLock()
	token := s.apiToken
	s.tokenMu.RUnlock()
	if token == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+token
}

func (s *APIServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/auth/status":
		s.handleAuthStatus(w, r)
	case r.URL.Path == "/api/auth/register":
		s.handleAuthRegister(w, r)
	case r.URL.Path == "/api/config":
		if !s.authorize(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		s.handleConfig(w, r)
	case r.URL.Path == "/api/rules":
		if !s.authorize(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		s.handleRules(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/rules/"):
		if !s.authorize(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		s.handleRule(w, r)
	default:
		writeError(w, http.StatusNotFound, "unknown endpoint")
	}
}

func (s *APIServer) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.tokenMu.RLock()
	configured := s.apiToken != ""
	envToken := s.envToken
	s.tokenMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":   configured,
		"can_register": !configured && !envToken,
	})
}

func (s *APIServer) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Registration is only allowed if no env token is set AND no token is configured.
	s.tokenMu.RLock()
	envToken := s.envToken
	configured := s.apiToken != ""
	s.tokenMu.RUnlock()
	if envToken {
		writeError(w, http.StatusConflict, "token is set via environment")
		return
	}
	if configured {
		writeError(w, http.StatusConflict, "token already configured")
		return
	}

	var payload struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	token := strings.TrimSpace(payload.Token)
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	if len(token) < 8 {
		writeError(w, http.StatusBadRequest, "token must be at least 8 characters")
		return
	}

	// Persist token in config, preserving rules.
	fc, err := loadConfig(s.configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		fc = FileConfig{Rules: []Rule{}}
	}
	fc.APIToken = token
	if err := saveConfig(s.configPath, fc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.tokenMu.Lock()
	s.apiToken = token
	s.tokenMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"configured": true})
}

func (s *APIServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Do not leak api token.
	writeJSON(w, http.StatusOK, s.manager.Config())
}

func (s *APIServer) handleRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var payload Rule
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rule, err := normalizeRule(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg, err := s.manager.UpsertRule(rule)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	s.tokenMu.RLock()
	fc := FileConfig{Rules: cfg.Rules, APIToken: s.apiToken}
	s.tokenMu.RUnlock()
	if err := saveConfig(s.configPath, fc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, rule)
}

func (s *APIServer) handleRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/rules/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing rule id")
		return
	}
	if !isValidRuleID(id) {
		writeError(w, http.StatusBadRequest, "invalid rule id")
		return
	}

	cfg, err := s.manager.DeleteRule(id)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	s.tokenMu.RLock()
	fc := FileConfig{Rules: cfg.Rules, APIToken: s.apiToken}
	s.tokenMu.RUnlock()
	if err := saveConfig(s.configPath, fc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func normalizeRule(rule Rule) (Rule, error) {
	rule.ID = strings.TrimSpace(rule.ID)
	rule.Name = strings.TrimSpace(rule.Name)
	rule.Protocol = strings.ToLower(strings.TrimSpace(rule.Protocol))
	rule.ListenHost = strings.TrimSpace(rule.ListenHost)
	rule.TargetType = strings.ToLower(strings.TrimSpace(rule.TargetType))
	rule.TargetHost = strings.TrimSpace(rule.TargetHost)
	rule.TargetContainer = strings.TrimSpace(rule.TargetContainer)
	rule.TargetNetwork = strings.TrimSpace(rule.TargetNetwork)

	if rule.Protocol == "" {
		rule.Protocol = "tcp"
	}
	if rule.Protocol != "tcp" && rule.Protocol != "udp" {
		return Rule{}, fmt.Errorf("invalid protocol %q", rule.Protocol)
	}

	if rule.ListenHost == "" {
		rule.ListenHost = "0.0.0.0"
	}
	if rule.ListenPort <= 0 || rule.ListenPort > 65535 {
		return Rule{}, fmt.Errorf("invalid listen port %d", rule.ListenPort)
	}

	if rule.TargetType == "" {
		if rule.TargetContainer != "" {
			rule.TargetType = "container"
		} else {
			rule.TargetType = "host"
		}
	}

	if rule.TargetPort <= 0 || rule.TargetPort > 65535 {
		return Rule{}, fmt.Errorf("invalid target port %d", rule.TargetPort)
	}

	switch rule.TargetType {
	case "container":
		if rule.TargetContainer == "" {
			return Rule{}, errors.New("target container is required")
		}
	case "host":
		if rule.TargetHost == "" {
			return Rule{}, errors.New("target host is required")
		}
	default:
		return Rule{}, fmt.Errorf("invalid target type %q", rule.TargetType)
	}

	if rule.ID == "" {
		rule.ID = newID()
	}
	if !isValidRuleID(rule.ID) {
		return Rule{}, errors.New("invalid rule id")
	}
	if rule.Name == "" {
		rule.Name = rule.ID
	}

	return rule, nil
}

func isValidRuleID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if r == '/' || r == '\\' {
			return false
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		switch r {
		case '-', '_', '.':
			continue
		default:
			return false
		}
	}
	return true
}

func rulesEqualForRunner(a, b Rule) bool {
	// Fields that influence listener binding or upstream resolution.
	return a.Protocol == b.Protocol &&
		a.ListenHost == b.ListenHost &&
		a.ListenPort == b.ListenPort &&
		a.TargetType == b.TargetType &&
		a.TargetHost == b.TargetHost &&
		a.TargetContainer == b.TargetContainer &&
		a.TargetNetwork == b.TargetNetwork &&
		a.TargetPort == b.TargetPort
}

func newID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("rule-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func loadConfig(path string) (FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, err
	}

	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

func saveConfig(path string, cfg FileConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o600)
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("empty request body")
	}
	return json.Unmarshal(data, v)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func applySecurityHeaders(w http.ResponseWriter) {
	// Minimal hardening for the embedded UI and JSON API.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func setConnDeadlineNow(c net.Conn) error {
	// Used to unblock io.Copy when one side completes.
	return c.SetDeadline(time.Now())
}

func main() {
	httpAddr := envOrDefault("PORTER_HTTP_ADDR", defaultHTTPAddr)
	dataDir := envOrDefault("PORTER_DATA_DIR", defaultDataDir)
	configPath := envOrDefault("PORTER_CONFIG_PATH", filepath.Join(dataDir, configFileName))
	apiTokenEnv := strings.TrimSpace(os.Getenv("PORTER_API_TOKEN"))
	envToken := apiTokenEnv != ""

	var resolver *DockerResolver
	if dockerResolver, err := NewDockerResolver(); err != nil {
		log.Printf("docker resolver disabled: %v", err)
	} else {
		resolver = dockerResolver
	}

	manager := NewManager(resolver)
	fileCfg, err := loadConfig(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("failed to load config: %v", err)
		}
		fileCfg = FileConfig{Rules: []Rule{}}
	}

	apiToken := apiTokenEnv
	if apiToken == "" {
		apiToken = strings.TrimSpace(fileCfg.APIToken)
	}

	if err := manager.ApplyConfig(Config{Rules: fileCfg.Rules}); err != nil {
		log.Printf("failed to apply config: %v", err)
	}

	h := NewAPIServer(configPath, manager, apiToken, envToken)

	httpServer := &http.Server{
		Addr:              httpAddr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("porter listening on %s", httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	_ = manager.ApplyConfig(Config{Rules: []Rule{}})
}
