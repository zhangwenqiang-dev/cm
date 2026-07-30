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
				return req.Host == req.Header.Get("Origin") || req.Header.Get("Origin") == "" || sameWebOrigin(req)
			},
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			_ = a.LogManager.Write(LogEntry{Level: "error", Action: "web.terminal", Profile: profile.Name, Message: err.Error()})
			return
		}
		startedAt := time.Now()
		op := a.operationContextForRequest(r)
		a.writeRuntimeLog(LogEntry{
			Action: "terminal.opened", Profile: profile.Name, AppleEmail: profile.AWS.AccountEmail,
			RequestID: op.RequestID, Source: "web-server", Phase: "opened", Outcome: "success", Message: "terminal.opened",
		})
		a.recordWebEvent(configPath, profile.Name, "terminal", true, webAPIResponse{OK: true, Output: "opened web terminal"})
		proxyErr := a.proxyWebTerminal(r.Context(), conn, profile)
		entry := LogEntry{
			Action: "terminal.closed", Profile: profile.Name, AppleEmail: profile.AWS.AccountEmail,
			RequestID: op.RequestID, Source: "web-server", Phase: "closed",
			DurationMS: time.Since(startedAt).Milliseconds(), Outcome: "success", Message: "terminal.closed",
		}
		if !normalTerminalClose(proxyErr) {
			classified := classifyOperationalError(proxyErr)
			entry.Level = classified.Level
			entry.ErrorCode = classified.Code
			entry.Outcome = "failure"
		}
		a.writeRuntimeLog(entry)
	}
}

func sameWebOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	writeOutput := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, data)
	}
	copyOutput := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, readErr := r.Read(buf)
			if n > 0 {
				if err := writeOutput(buf[:n]); err != nil {
					cancel()
					return
				}
			}
			if readErr != nil {
				cancel()
				return
			}
		}
	}
	go copyOutput(stdout)
	go copyOutput(stderr)
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- session.Wait()
		cancel()
	}()
	readDone := make(chan error, 1)
	go func() {
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				readDone <- err
				cancel()
				return
			}
			if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
				continue
			}
			if _, err := stdin.Write(data); err != nil {
				readDone <- err
				cancel()
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-waitDone:
		return err
	case err := <-readDone:
		return err
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
