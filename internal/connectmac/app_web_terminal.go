package connectmac

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type webTerminalProfile struct {
	Profile    string `json:"profile"`
	AppleEmail string `json:"apple_email,omitempty"`
	Target     string `json:"target"`
	Ready      bool   `json:"ready"`
}

func (a App) webTerminalCheckHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		profile, err := a.prepareWebTerminal(r, configPath, r.URL.Query().Get("profile"))
		if err != nil {
			writeWebJSON(w, webAPIResponse{OK: false, Code: 1, Error: err.Error()})
			return
		}
		writeWebJSON(w, webAPIResponse{OK: true, Data: webTerminalProfile{
			Profile:    profile.Name,
			AppleEmail: profile.AWS.AccountEmail,
			Target:     fmt.Sprintf("%s@%s", profile.User, profile.Host),
			Ready:      true,
		}})
	}
}

func (a App) webTerminalWSHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWebError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		profile, err := a.prepareWebTerminal(r, configPath, r.URL.Query().Get("profile"))
		if err != nil {
			writeWebError(w, http.StatusBadRequest, err.Error())
			return
		}
		upgrader := websocket.Upgrader{
			CheckOrigin: func(req *http.Request) bool {
				return sameWebOrigin(req)
			},
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			op := a.operationContextForRequest(r)
			classified := classifyLocalOperationError(err)
			a.writeRuntimeLog(LogEntry{
				Level: classified.Level, Action: "terminal.failed", Operation: "terminal",
				Profile: profile.Name, AppleEmail: profile.AWS.AccountEmail,
				ActorMemberID: op.Actor.MemberID, ActorMemberEmail: op.Actor.MemberEmail,
				ActorMemberName: op.Actor.MemberName, RequestID: op.RequestID,
				SessionIDHash: op.SessionIDHash, Source: "web-server",
				Phase: "upgrade", FailureStage: "websocket", ErrorCode: classified.Code,
				ExitCode: classified.ExitCode, Outcome: "failure", Message: classified.Detail,
			})
			return
		}
		startedAt := time.Now()
		op := a.operationContextForRequest(r)
		a.writeRuntimeLog(LogEntry{
			Action: "terminal.opened", Profile: profile.Name, AppleEmail: profile.AWS.AccountEmail,
			ActorMemberID: op.Actor.MemberID, ActorMemberEmail: op.Actor.MemberEmail,
			ActorMemberName: op.Actor.MemberName, RequestID: op.RequestID,
			SessionIDHash: op.SessionIDHash, Source: "web-server",
			Phase: "opened", Outcome: "success", Message: "terminal.opened",
		})
		a.recordWebEventForRequest(r, configPath, profile.Name, "terminal", true, webAPIResponse{OK: true, Output: "opened web terminal"})
		proxyErr := a.proxyWebTerminal(r.Context(), conn, profile)
		entry := LogEntry{
			Action: "terminal.closed", Profile: profile.Name, AppleEmail: profile.AWS.AccountEmail,
			ActorMemberID: op.Actor.MemberID, ActorMemberEmail: op.Actor.MemberEmail,
			ActorMemberName: op.Actor.MemberName, RequestID: op.RequestID,
			SessionIDHash: op.SessionIDHash, Source: "web-server", Phase: "closed",
			DurationMS: positiveDurationMS(time.Since(startedAt)), Outcome: "success",
			Message: "terminal.closed reason=normal",
		}
		entry = finalizeTerminalClosedEntry(entry, proxyErr)
		a.writeRuntimeLog(entry)
	}
}

func sameWebOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	return origin == "http://"+r.Host || origin == "https://"+r.Host
}

func (a App) prepareWebTerminal(r *http.Request, configPath, profileRef string) (Profile, error) {
	profileRef = strings.TrimSpace(profileRef)
	if profileRef == "" {
		return Profile{}, errors.New("profile is required")
	}
	cfg, err := a.loadWebConfig(r, configPath)
	if err != nil {
		return Profile{}, err
	}
	profile, err := resolveProfileRef(cfg, profileRef)
	if err != nil {
		return Profile{}, err
	}
	if errs := a.Validator.ValidateAccess(profile); len(errs) > 0 {
		return Profile{}, fmt.Errorf("profile %s config error:\n%s", profile.Name, strings.Join(validationMessages(errs), "\n"))
	}
	if errs := a.Validator.ValidateAWSProfile(profile); len(errs) > 0 {
		return Profile{}, fmt.Errorf("profile %s aws config error:\n%s", profile.Name, strings.Join(validationMessages(errs), "\n"))
	}
	_, status, err := a.AWSService.StatusWithOptions(r.Context(), profile, AWSStatusOptions{IncludeTerminal: false})
	if err != nil {
		return Profile{}, fmt.Errorf("aws status failed: %w", err)
	}
	if !AWSStatusReady(status) {
		return Profile{}, fmt.Errorf("aws mac is not ready: %s", AWSReadinessSummary(status))
	}
	check, err := a.fixHostKey(r.Context(), profile)
	if err != nil {
		return Profile{}, err
	}
	if check.Status == HostKeyScanFailed {
		return Profile{}, fmt.Errorf("ssh host key scan failed for %s: %s", profile.Host, check.Message)
	}
	return profile, nil
}

func (a App) proxyWebTerminal(ctx context.Context, conn *websocket.Conn, profile Profile) error {
	defer conn.Close()
	client, err := a.openSSHTerminalClient(profile)
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\nconnect failed: "+err.Error()+"\r\n"))
		return err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\nnew session failed: "+err.Error()+"\r\n"))
		return err
	}
	defer session.Close()
	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return err
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 40, 120, modes); err != nil {
		return err
	}
	if err := session.Shell(); err != nil {
		return err
	}
	return proxyTerminalIO(ctx, conn, stdin, stdout, stderr, session.Wait)
}

type terminalWebSocket interface {
	ReadMessage() (messageType int, data []byte, err error)
	WriteMessage(messageType int, data []byte) error
}

func proxyTerminalIO(
	ctx context.Context,
	conn terminalWebSocket,
	stdin io.Writer,
	stdout io.Reader,
	stderr io.Reader,
	wait func() error,
) error {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	results := make(chan error, 4)
	report := func(err error) {
		results <- err
		if err != nil {
			cancel(err)
		}
	}
	var writeMu sync.Mutex
	writeOutput := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, data)
	}
	copyOutput := func(name string, reader io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				if err := writeOutput(buf[:n]); err != nil {
					report(fmt.Errorf("terminal websocket write: %w", err))
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					report(fmt.Errorf("terminal %s read: %w", name, readErr))
				}
				return
			}
		}
	}
	go copyOutput("stdout", stdout)
	go copyOutput("stderr", stderr)
	go func() {
		err := wait()
		if err != nil {
			report(fmt.Errorf("terminal ssh session wait: %w", err))
			return
		}
		report(nil)
	}()
	go func() {
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				report(err)
				return
			}
			if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
				continue
			}
			if _, err := stdin.Write(data); err != nil {
				report(fmt.Errorf("terminal ssh stdin write: %w", err))
				return
			}
		}
	}()

	select {
	case err := <-results:
		return err
	default:
	}
	select {
	case err := <-results:
		return err
	case <-runCtx.Done():
		select {
		case err := <-results:
			return err
		default:
		}
		return context.Cause(runCtx)
	}
}

func (a App) openSSHTerminalClient(profile Profile) (*ssh.Client, error) {
	keyPath, err := ExpandPath(profile.IdentityFile)
	if err != nil {
		return nil, err
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("parse identity file %s: %w", profile.IdentityFile, err)
	}
	knownHostsPath := a.KnownHosts
	if knownHostsPath == "" {
		knownHostsPath = "~/.ssh/known_hosts"
	}
	knownHostsPath, err = ExpandPath(knownHostsPath)
	if err != nil {
		return nil, err
	}
	hostKeyCallback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{
		User:            profile.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}
	return ssh.Dial("tcp", net.JoinHostPort(profile.Host, "22"), config)
}
