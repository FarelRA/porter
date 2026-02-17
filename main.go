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
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

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

type Manager struct {
	mu       sync.Mutex
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
	m.mu.Lock()
	prevCfg := m.cfg
	prevRunners := m.runners
	m.cfg = cfg
	m.runners = map[string]Runner{}
	m.mu.Unlock()

	for _, runner := range prevRunners {
		_ = runner.Stop()
	}

	err := m.startEnabled(cfg)
	if err == nil {
		return nil
	}

	for _, runner := range m.runners {
		_ = runner.Stop()
	}

	m.mu.Lock()
	m.cfg = prevCfg
	m.runners = map[string]Runner{}
	m.mu.Unlock()
	_ = m.startEnabled(prevCfg)

	return err
}

func (m *Manager) UpsertRule(rule Rule) (Config, error) {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

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
	cfg := m.cfg
	m.mu.Unlock()

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
			defer c.Close()

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

			done := make(chan struct{})
			go func() {
				_, _ = io.Copy(upstream, c)
				close(done)
			}()

			_, _ = io.Copy(c, upstream)
			<-done
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
		conn:         conn,
		stop:         make(chan struct{}),
		resolve:      resolve,
		clients:      map[string]*udpClient{},
		idleTimeout:  2 * time.Minute,
		cleanupEvery: 30 * time.Second,
	}

	runner.wg.Add(2)
	go runner.serve()
	go runner.cleanup()

	return runner, nil
}

func (r *udpRunner) serve() {
	defer r.wg.Done()
	buf := make([]byte, 65535)

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

		targetAddr, err := net.ResolveUDPAddr("udp", target)
		if err != nil {
			log.Printf("udp target error: %v", err)
			continue
		}

		clientKey := addr.String()
		client := r.getClient(clientKey, targetAddr, addr)
		if client == nil || client.conn == nil {
			continue
		}
		client.lastSeen = time.Now()

		if _, err := client.conn.Write(buf[:n]); err != nil {
			log.Printf("udp write error: %v", err)
		}
	}
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
		buf := make([]byte, 65535)
		for {
			n, err := upstream.Read(buf)
			if err != nil {
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

	containers, listErr := d.cli.ContainerList(ctx, types.ContainerListOptions{All: true})
	if listErr != nil {
		return types.ContainerJSON{}, err
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
}

func NewAPIServer(configPath string, manager *Manager) *APIServer {
	webRoot, _ := fs.Sub(embeddedWeb, "web")
	return &APIServer{
		configPath: configPath,
		manager:    manager,
		files:      http.FileServer(http.FS(webRoot)),
	}
}

func (s *APIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.handleAPI(w, r)
		return
	}
	s.files.ServeHTTP(w, r)
}

func (s *APIServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/config":
		s.handleConfig(w, r)
	case r.URL.Path == "/api/rules":
		s.handleRules(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/rules/"):
		s.handleRule(w, r)
	default:
		writeError(w, http.StatusNotFound, "unknown endpoint")
	}
}

func (s *APIServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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

	if err := saveConfig(s.configPath, cfg); err != nil {
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

	cfg, err := s.manager.DeleteRule(id)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	if err := saveConfig(s.configPath, cfg); err != nil {
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
	if rule.Name == "" {
		rule.Name = rule.ID
	}

	return rule, nil
}

func newID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("rule-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func saveConfig(path string, cfg Config) error {
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

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	httpAddr := envOrDefault("PORTER_HTTP_ADDR", defaultHTTPAddr)
	dataDir := envOrDefault("PORTER_DATA_DIR", defaultDataDir)
	configPath := envOrDefault("PORTER_CONFIG_PATH", filepath.Join(dataDir, configFileName))

	var resolver *DockerResolver
	if dockerResolver, err := NewDockerResolver(); err != nil {
		log.Printf("docker resolver disabled: %v", err)
	} else {
		resolver = dockerResolver
	}

	manager := NewManager(resolver)
	cfg, err := loadConfig(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("failed to load config: %v", err)
		}
		cfg = Config{Rules: []Rule{}}
	}

	if err := manager.ApplyConfig(cfg); err != nil {
		log.Printf("failed to apply config: %v", err)
	}

	server := NewAPIServer(configPath, manager)

	log.Printf("porter listening on %s", httpAddr)
	if err := http.ListenAndServe(httpAddr, server); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
