package sshCmd

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cloudfoundry-incubator/diego-ssh/helpers"
	"github.com/cloudfoundry/cli/cf/models"
	"github.com/cloudfoundry/cli/cf/ssh/options"
	"github.com/cloudfoundry/cli/cf/ssh/sigwinch"
	"github.com/cloudfoundry/cli/cf/ssh/terminal"
	"github.com/docker/docker/pkg/term"
)

//go:generate counterfeiter -o fakes/fake_secure_shell.go . SecureShell
type SecureShell interface {
	Connect(opts *options.SSHOptions) error
	InteractiveSession() error
	LocalPortForward() error
	Wait() error
	Close() error
}

//go:generate counterfeiter -o fakes/fake_secure_dialer.go . SecureDialer
type SecureDialer interface {
	Dial(network, address string, config *ssh.ClientConfig) (SecureClient, error)
}

//go:generate counterfeiter -o fakes/fake_secure_client.go . SecureClient
type SecureClient interface {
	NewSession() (SecureSession, error)
	Conn() ssh.Conn
	Dial(network, address string) (net.Conn, error)
	Wait() error
	Close() error
}

//go:generate counterfeiter -o fakes/fake_listener_factory.go . ListenerFactory
type ListenerFactory interface {
	Listen(network, address string) (net.Listener, error)
}

//go:generate counterfeiter -o fakes/fake_secure_session.go . SecureSession
type SecureSession interface {
	RequestPty(term string, height, width int, termModes ssh.TerminalModes) error
	SendRequest(name string, wantReply bool, payload []byte) (bool, error)
	StdinPipe() (io.WriteCloser, error)
	StdoutPipe() (io.Reader, error)
	StderrPipe() (io.Reader, error)
	Start(command string) error
	Shell() error
	Wait() error
	Close() error
}

type secureShell struct {
	secureDialer           SecureDialer
	terminalHelper         sshTerminal.TerminalHelper
	listenerFactory        ListenerFactory
	keepAliveInterval      time.Duration
	app                    models.Application
	sshEndpointFingerprint string
	sshEndpoint            string
	token                  string
	secureClient           SecureClient
	opts                   *options.SSHOptions

	localListeners []net.Listener
}

func NewSecureShell(
	secureDialer SecureDialer,
	terminalHelper sshTerminal.TerminalHelper,
	listenerFactory ListenerFactory,
	keepAliveInterval time.Duration,
	app models.Application,
	sshEndpointFingerprint string,
	sshEndpoint string,
	token string,
) SecureShell {
	return &secureShell{
		secureDialer:      secureDialer,
		terminalHelper:    terminalHelper,
		listenerFactory:   listenerFactory,
		keepAliveInterval: keepAliveInterval,
		app:               app,
		sshEndpointFingerprint: sshEndpointFingerprint,
		sshEndpoint:            sshEndpoint,
		token:                  token,
		localListeners:         []net.Listener{},
	}
}

func (c *secureShell) Connect(opts *options.SSHOptions) error {
	err := c.validateTarget(opts)
	if err != nil {
		return err
	}

	clientConfig := &ssh.ClientConfig{
		User: fmt.Sprintf("cf:%s/%d", c.app.Guid, opts.Index),
		Auth: []ssh.AuthMethod{
			ssh.Password(c.token),
		},
		HostKeyCallback: fingerprintCallback(opts, c.sshEndpointFingerprint),
	}

	secureClient, err := c.secureDialer.Dial("tcp", c.sshEndpoint, clientConfig)
	if err != nil {
		return err
	}

	c.secureClient = secureClient
	c.opts = opts
	return nil
}

func (c *secureShell) Close() error {
	for _, listener := range c.localListeners {
		listener.Close()
	}
	return c.secureClient.Close()
}

func (c *secureShell) LocalPortForward() error {
	for _, forwardSpec := range c.opts.ForwardSpecs {
		listener, err := c.listenerFactory.Listen("tcp", forwardSpec.ListenAddress)
		if err != nil {
			return err
		}
		c.localListeners = append(c.localListeners, listener)

		go c.localForwardAcceptLoop(listener, forwardSpec.ConnectAddress)
	}

	return nil
}

func (c *secureShell) localForwardAcceptLoop(listener net.Listener, addr string) {
	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return
		}

		go c.handleForwardConnection(conn, addr)
	}
}

func (c *secureShell) handleForwardConnection(conn net.Conn, targetAddr string) {
	defer conn.Close()

	target, err := c.secureClient.Dial("tcp", targetAddr)
	if err != nil {
		fmt.Printf("connect to %s failed: %s\n", targetAddr, err.Error())
		return
	}
	defer target.Close()

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go copyAndClose(wg, conn, target)
	go copyAndClose(wg, target, conn)
	wg.Wait()
}

func copyAndClose(wg *sync.WaitGroup, dest io.WriteCloser, src io.Reader) {
	io.Copy(dest, src)
	dest.Close()
	if wg != nil {
		wg.Done()
	}
}

func (c *secureShell) InteractiveSession() error {
	var err error

	secureClient := c.secureClient
	opts := c.opts

	session, err := secureClient.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session allocation failed: %s", err.Error())
	}
	defer session.Close()

	stdin, stdout, stderr := c.terminalHelper.StdStreams()

	inPipe, err := session.StdinPipe()
	if err != nil {
		return err
	}

	outPipe, err := session.StdoutPipe()
	if err != nil {
		return err
	}

	errPipe, err := session.StderrPipe()
	if err != nil {
		return err
	}

	stdinFd, stdinIsTerminal := c.terminalHelper.GetFdInfo(stdin)
	stdoutFd, stdoutIsTerminal := c.terminalHelper.GetFdInfo(stdout)

	if c.shouldAllocateTerminal(opts, stdinIsTerminal) {
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 115200,
			ssh.TTY_OP_OSPEED: 115200,
		}

		width, height := c.getWindowDimensions(stdoutFd)

		err = session.RequestPty(c.terminalType(), height, width, modes)
		if err != nil {
			return err
		}

		var state *term.State
		state, err = c.terminalHelper.SetRawTerminal(stdinFd)
		if err == nil {
			defer c.terminalHelper.RestoreTerminal(stdinFd, state)
		}
	}

	if len(opts.Command) != 0 {
		cmd := strings.Join(opts.Command, " ")
		err = session.Start(cmd)
		if err != nil {
			return err
		}
	} else {
		err = session.Shell()
		if err != nil {
			return err
		}
	}

	go copyAndClose(nil, inPipe, stdin)
	go io.Copy(stdout, outPipe)
	go io.Copy(stderr, errPipe)

	if stdoutIsTerminal {
		resized := make(chan os.Signal, 16)

		if runtime.GOOS == "windows" {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()

			go func() {
				for _ = range ticker.C {
					resized <- syscall.Signal(-1)
				}
				close(resized)
			}()
		} else {
			signal.Notify(resized, sigwinch.SIGWINCH())
			defer func() { signal.Stop(resized); close(resized) }()
		}

		go c.resize(resized, session, stdoutFd)
	}

	keepaliveStopCh := make(chan struct{})
	defer close(keepaliveStopCh)

	go keepalive(secureClient.Conn(), time.NewTicker(c.keepAliveInterval), keepaliveStopCh)

	return session.Wait()
}

func (c *secureShell) Wait() error {
	return c.secureClient.Wait()
}

func (c *secureShell) validateTarget(opts *options.SSHOptions) error {
	if strings.ToUpper(c.app.State) != "STARTED" {
		return fmt.Errorf("Application %q is not in the STARTED state", opts.AppName)
	}

	if !c.app.Diego {
		return fmt.Errorf("Application %q is not running on Diego", opts.AppName)
	}

	return nil
}

type hostKeyCallback func(hostname string, remote net.Addr, key ssh.PublicKey) error

func fingerprintCallback(opts *options.SSHOptions, expectedFingerprint string) hostKeyCallback {
	if opts.SkipHostValidation {
		return nil
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		switch len(expectedFingerprint) {
		case helpers.SHA1_FINGERPRINT_LENGTH:
			fingerprint := helpers.SHA1Fingerprint(key)
			if fingerprint != expectedFingerprint {
				return fmt.Errorf("Host key verification failed.\n\nThe fingerprint of the received key was %q.", fingerprint)
			}
		case helpers.MD5_FINGERPRINT_LENGTH:
			fingerprint := helpers.MD5Fingerprint(key)
			if fingerprint != expectedFingerprint {
				return fmt.Errorf("Host key verification failed.\n\nThe fingerprint of the received key was %q.", fingerprint)
			}
		case 0:
			fingerprint := helpers.MD5Fingerprint(key)
			return fmt.Errorf("Unable to verify identity of host.\n\nThe fingerprint of the received key was %q.", fingerprint)
		default:
			return errors.New("Unsupported host key fingerprint format")
		}
		return nil
	}
}

func (c *secureShell) shouldAllocateTerminal(opts *options.SSHOptions, stdinIsTerminal bool) bool {
	switch opts.TerminalRequest {
	case options.REQUEST_TTY_FORCE:
		return true
	case options.REQUEST_TTY_NO:
		return false
	case options.REQUEST_TTY_YES:
		return stdinIsTerminal
	case options.REQUEST_TTY_AUTO:
		return len(opts.Command) == 0 && stdinIsTerminal
	default:
		return false
	}
}

func (c *secureShell) resize(resized <-chan os.Signal, session SecureSession, terminalFd uintptr) {
	type resizeMessage struct {
		Width       uint32
		Height      uint32
		PixelWidth  uint32
		PixelHeight uint32
	}

	var previousWidth, previousHeight int

	for _ = range resized {
		width, height := c.getWindowDimensions(terminalFd)

		if width == previousWidth && height == previousHeight {
			continue
		}

		message := resizeMessage{
			Width:  uint32(width),
			Height: uint32(height),
		}

		session.SendRequest("window-change", false, ssh.Marshal(message))

		previousWidth = width
		previousHeight = height
	}
}

func keepalive(conn ssh.Conn, ticker *time.Ticker, stopCh chan struct{}) {
	for {
		select {
		case <-ticker.C:
			conn.SendRequest("keepalive@cloudfoundry.org", true, nil)
		case <-stopCh:
			ticker.Stop()
			return
		}
	}
}

func (c *secureShell) terminalType() string {
	term := os.Getenv("TERM")
	if term == "" {
		term = "xterm"
	}
	return term
}

func (c *secureShell) getWindowDimensions(terminalFd uintptr) (width int, height int) {
	winSize, err := c.terminalHelper.GetWinsize(terminalFd)
	if err != nil {
		winSize = &term.Winsize{
			Width:  80,
			Height: 43,
		}
	}

	return int(winSize.Width), int(winSize.Height)
}

type secureDialer struct{}

func (d *secureDialer) Dial(network string, address string, config *ssh.ClientConfig) (SecureClient, error) {
	client, err := ssh.Dial(network, address, config)
	if err != nil {
		return nil, err
	}

	return &secureClient{client: client}, nil
}

func DefaultSecureDialer() SecureDialer {
	return &secureDialer{}
}

type secureClient struct{ client *ssh.Client }

func (sc *secureClient) Close() error   { return sc.client.Close() }
func (sc *secureClient) Conn() ssh.Conn { return sc.client.Conn }
func (sc *secureClient) Wait() error    { return sc.client.Wait() }
func (sc *secureClient) Dial(n, addr string) (net.Conn, error) {
	return sc.client.Dial(n, addr)
}
func (sc *secureClient) NewSession() (SecureSession, error) {
	return sc.client.NewSession()
}

type listenerFactory struct{}

func (lf *listenerFactory) Listen(network, address string) (net.Listener, error) {
	return net.Listen(network, address)
}

func DefaultListenerFactory() ListenerFactory {
	return &listenerFactory{}
}
