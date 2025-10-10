package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// EventReason identifies the type of state transition emitted by the connection manager.
type EventReason string

const (
	// ReasonReboot indicates the remote router rebooted (uptime reset).
	ReasonReboot EventReason = "reboot"
	// ReasonConnectionDrop indicates the SSH transport was lost.
	ReasonConnectionDrop EventReason = "connection_drop"
	// ReasonReconnectSuccess indicates the manager re-established the SSH session after a drop.
	ReasonReconnectSuccess EventReason = "reconnect_success"
)

// Event represents a notification emitted to registered handlers when connection state changes.
type Event struct {
	Reason EventReason
	Err    error
}

// EventHandler consumes connection state transition events.
type EventHandler func(Event)

// DialFunc abstracts ssh.Dial for testing.
type DialFunc func(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error)

// CommandRunner executes a remote command against an active SSH client.
type CommandRunner interface {
	Run(ctx context.Context, client *ssh.Client, command string) ([]byte, error)
}

// RouterConfig captures authentication and runtime settings for a router.
type RouterConfig struct {
	Address           string
	Username          string
	AuthMethod        ssh.AuthMethod
	HostKeyCallback   ssh.HostKeyCallback
	Timeout           time.Duration
	KeepAliveInterval time.Duration
	KeepAliveCommand  string
	Dial              DialFunc
	Runner            CommandRunner
	Logger            logr.Logger
	MaxBackoff        time.Duration
	InitialBackoff    time.Duration
}

// Validate ensures the configuration is well formed.
func (c *RouterConfig) Validate() error {
	if c == nil {
		return errors.New("router config is nil")
	}
	if c.Address == "" {
		return errors.New("address must be provided")
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return fmt.Errorf("address %q must be host:port: %w", c.Address, err)
	}
	if c.Username == "" {
		return errors.New("username must be provided")
	}
	if c.AuthMethod == nil {
		return errors.New("auth method must be provided")
	}
	return nil
}

// sshCommandRunner implements CommandRunner using ssh.Session.
type sshCommandRunner struct{}

func (sshCommandRunner) Run(ctx context.Context, client *ssh.Client, command string) ([]byte, error) {
	if client == nil {
		return nil, errors.New("ssh client is nil")
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = session.Close()
	}()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if err := session.Start(command); err != nil {
		return nil, err
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			if msg := stderr.String(); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
			return nil, err
		}
		return stdout.Bytes(), nil
	case <-ctx.Done():
		_ = session.Signal(ssh.Signal("KILL"))
		_ = session.Close()
		<-done
		return nil, ctx.Err()
	}
}

// SSHConnectionManager manages a persistent SSH connection and monitors keep-alives.
type SSHConnectionManager struct {
	config RouterConfig

	mu           sync.RWMutex
	conn         *ssh.Client
	connected    bool
	reconnecting bool
	lastUptime   float64
	started      bool

	handlers   map[uint64]EventHandler
	handlerMu  sync.RWMutex
	handlerSeq uint64

	baseCtx    context.Context
	baseCancel context.CancelFunc
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup

	dial   DialFunc
	runner CommandRunner

	log logr.Logger

	metrics *Metrics
}

// NewSSHConnectionManager constructs a manager from config.
func NewSSHConnectionManager(cfg RouterConfig) (*SSHConnectionManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	logger := log.Log.WithName("ssh-manager").WithValues("router", cfg.Address)
	if !cfg.Logger.IsZero() {
		logger = cfg.Logger.WithValues("router", cfg.Address)
	}

	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.KeepAliveInterval == 0 {
		cfg.KeepAliveInterval = 30 * time.Second
	}
	if cfg.KeepAliveCommand == "" {
		cfg.KeepAliveCommand = "cat /proc/uptime"
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 2 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = time.Minute
	}

	dial := ssh.Dial
	if cfg.Dial != nil {
		dial = cfg.Dial
	}
	runner := CommandRunner(sshCommandRunner{})
	if cfg.Runner != nil {
		runner = cfg.Runner
	}

	return &SSHConnectionManager{
		config:   cfg,
		handlers: make(map[uint64]EventHandler),
		dial:     dial,
		runner:   runner,
		log:      logger,
		metrics:  defaultMetrics(),
	}, nil
}

// Start establishes the connection and begins monitoring.
func (m *SSHConnectionManager) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context must not be nil")
	}

	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("ssh manager already started")
	}
	m.started = true
	m.baseCtx, m.baseCancel = context.WithCancel(ctx)
	m.ctx, m.cancel = context.WithCancel(m.baseCtx)
	m.mu.Unlock()

	if err := m.establishConnection(); err != nil {
		m.log.Error(err, "initial SSH connection failed")
		m.transitionToReconnecting(err)
		return fmt.Errorf("initial connection failed, reconnecting in background: %w", err)
	}

	m.wg.Add(1)
	go m.keepAliveLoop()
	return nil
}

func (m *SSHConnectionManager) establishConnection() error {
	hostKeyCallback := m.config.HostKeyCallback
	if hostKeyCallback == nil {
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
		m.log.Info("using insecure host key verification")
	}

	sshCfg := &ssh.ClientConfig{
		User:            m.config.Username,
		Auth:            []ssh.AuthMethod{m.config.AuthMethod},
		HostKeyCallback: hostKeyCallback,
		Timeout:         m.config.Timeout,
	}

	conn, err := m.dial("tcp", m.config.Address, sshCfg)
	if err != nil {
		return fmt.Errorf("ssh dial failed: %w", err)
	}

	m.mu.Lock()
	oldConn := m.conn
	wasConnected := m.connected
	m.conn = conn
	m.connected = true
	m.reconnecting = false
	m.mu.Unlock()

	if oldConn != nil {
		_ = oldConn.Close()
	}
	if !wasConnected {
		m.metrics.IncActive()
	}
	m.metrics.IncReconnects()
	m.log.Info("SSH connection established")
	m.notifyHandlers(Event{Reason: ReasonReconnectSuccess})
	return nil
}

func (m *SSHConnectionManager) keepAliveLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(m.config.KeepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := m.checkConnection(); err != nil {
				m.log.Error(err, "keep-alive failed")
				m.handleConnectionDrop(err)
				return
			}
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *SSHConnectionManager) checkConnection() error {
	m.mu.RLock()
	ctx := m.ctx
	m.mu.RUnlock()
	if ctx == nil {
		return errors.New("manager context unavailable")
	}

	cmdCtx, cancel := context.WithTimeout(ctx, m.config.Timeout)
	defer cancel()

	output, err := m.RunCommand(cmdCtx, m.config.KeepAliveCommand)
	if err != nil {
		return err
	}

	uptime, err := parseUptime(output)
	if err != nil {
		return err
	}

	m.mu.Lock()
	previous := m.lastUptime
	m.lastUptime = uptime
	m.mu.Unlock()

	if previous > 0 && uptime+1 < previous {
		m.log.Info("router reboot detected", "previous", previous, "current", uptime)
		m.notifyHandlers(Event{Reason: ReasonReboot})
	}

	return nil
}

func (m *SSHConnectionManager) transitionToReconnecting(err error) {
	m.mu.Lock()
	if m.reconnecting {
		m.mu.Unlock()
		return
	}
	m.reconnecting = true
	wasConnected := m.connected
	m.connected = false
	conn := m.conn
	m.conn = nil
	cancel := m.cancel
	m.ctx = nil
	m.cancel = nil
	baseCtx := m.baseCtx
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	if wasConnected {
		m.metrics.DecActive()
	}
	m.metrics.IncDrops()

	m.notifyHandlers(Event{Reason: ReasonConnectionDrop, Err: err})

	m.wg.Add(1)
	go m.reconnectLoop(baseCtx)
}

func (m *SSHConnectionManager) handleConnectionDrop(err error) {
	m.transitionToReconnecting(err)
}

func (m *SSHConnectionManager) reconnectLoop(parent context.Context) {
	defer m.wg.Done()

	if parent == nil {
		parent = context.Background()
	}

	backoff := m.config.InitialBackoff
	for {
		select {
		case <-parent.Done():
			return
		default:
		}

		if err := m.establishConnection(); err != nil {
			m.log.Error(err, "reconnect attempt failed", "backoff", backoff)
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-parent.Done():
				timer.Stop()
				return
			}
			backoff *= 2
			if backoff > m.config.MaxBackoff {
				backoff = m.config.MaxBackoff
			}
			continue
		}

		m.mu.Lock()
		if m.ctx == nil || m.cancel == nil {
			m.ctx, m.cancel = context.WithCancel(parent)
		}
		m.reconnecting = false
		m.mu.Unlock()

		m.wg.Add(1)
		go m.keepAliveLoop()
		return
	}
}

// RunCommand executes a shell command on the router using the managed SSH connection.
func (m *SSHConnectionManager) RunCommand(ctx context.Context, command string) ([]byte, error) {
	if ctx == nil {
		m.mu.RLock()
		ctx = m.ctx
		m.mu.RUnlock()
	}
	if ctx == nil {
		return nil, errors.New("manager not started")
	}

	m.mu.RLock()
	client := m.conn
	m.mu.RUnlock()
	if client == nil {
		return nil, errors.New("ssh connection not established")
	}

	return m.runner.Run(ctx, client, command)
}

// Close terminates the manager and underlying connection.
func (m *SSHConnectionManager) Close() error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	cancel := m.cancel
	baseCancel := m.baseCancel
	m.cancel = nil
	m.baseCancel = nil
	m.baseCtx = nil
	m.started = false
	conn := m.conn
	m.conn = nil
	wasConnected := m.connected
	m.connected = false
	m.ctx = nil
	m.reconnecting = false
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if baseCancel != nil {
		baseCancel()
	}
	m.wg.Wait()

	if conn != nil {
		_ = conn.Close()
	}
	if wasConnected {
		m.metrics.DecActive()
	}
	return nil
}

// IsConnected returns true when the SSH connection is active.
func (m *SSHConnectionManager) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connected
}

// IsReconnecting indicates whether the manager is attempting to re-establish the session.
func (m *SSHConnectionManager) IsReconnecting() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reconnecting
}

// RegisterHandler registers a callback to receive state events.
func (m *SSHConnectionManager) RegisterHandler(handler EventHandler) uint64 {
	if handler == nil {
		return 0
	}
	m.handlerMu.Lock()
	defer m.handlerMu.Unlock()
	m.handlerSeq++
	id := m.handlerSeq
	m.handlers[id] = handler
	return id
}

// UnregisterHandler removes a previously registered handler.
func (m *SSHConnectionManager) UnregisterHandler(id uint64) {
	if id == 0 {
		return
	}
	m.handlerMu.Lock()
	defer m.handlerMu.Unlock()
	delete(m.handlers, id)
}

// HandlerCount returns the number of registered handlers.
func (m *SSHConnectionManager) HandlerCount() int {
	m.handlerMu.RLock()
	defer m.handlerMu.RUnlock()
	return len(m.handlers)
}

func (m *SSHConnectionManager) notifyHandlers(event Event) {
	m.handlerMu.RLock()
	handlers := make([]EventHandler, 0, len(m.handlers))
	for _, handler := range m.handlers {
		handlers = append(handlers, handler)
	}
	m.handlerMu.RUnlock()

	for _, handler := range handlers {
		go handler(event)
	}
}

// parseUptime parses /proc/uptime output to seconds.
func parseUptime(out []byte) (float64, error) {
	fields := bytes.Fields(out)
	if len(fields) == 0 {
		return 0, errors.New("empty uptime output")
	}
	return parseFloat(fields[0])
}

func parseFloat(value []byte) (float64, error) {
	v, err := strconvParseFloat(string(bytes.TrimSpace(value)))
	if err != nil {
		return 0, fmt.Errorf("parse uptime: %w", err)
	}
	return v, nil
}

// wrapper to allow overriding in tests.
var strconvParseFloat = func(value string) (float64, error) {
	return strconv.ParseFloat(value, 64)
}

// Metrics tracks manager level counters.
type Metrics struct {
	active     atomic.Int64
	drops      atomic.Int64
	reconnects atomic.Int64
}

func defaultMetrics() *Metrics {
	return &Metrics{}
}

func (m *Metrics) IncActive() {
	if m != nil {
		m.active.Add(1)
	}
}

func (m *Metrics) DecActive() {
	if m != nil {
		m.active.Add(-1)
	}
}

func (m *Metrics) IncDrops() {
	if m != nil {
		m.drops.Add(1)
	}
}

func (m *Metrics) IncReconnects() {
	if m != nil {
		m.reconnects.Add(1)
	}
}

// Snapshot exposes current metrics counters.
type Snapshot struct {
	Active     int64
	Drops      int64
	Reconnects int64
}

// Snapshot returns the metrics snapshot.
func (m *Metrics) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	return Snapshot{
		Active:     m.active.Load(),
		Drops:      m.drops.Load(),
		Reconnects: m.reconnects.Load(),
	}
}
