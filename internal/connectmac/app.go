package connectmac

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"
)

type Runner interface {
	RunForeground(ctx context.Context, args []string) error
	StartBackground(ctx context.Context, args []string) (int, error)
	Stop(pid int) error
	RunRsync(ctx context.Context, args []string) error
	RunRsyncProgress(ctx context.Context, args []string, onOutput func(string)) error
	RunRsyncCommandProgress(ctx context.Context, path string, args []string, onOutput func(string)) error
	RsyncCommandOutput(ctx context.Context, path string, args []string) ([]byte, error)
	KnownHostKey(ctx context.Context, host string) (string, error)
	ScanHostKey(ctx context.Context, host string) (string, error)
	ForgetHost(ctx context.Context, host string) error
	OpenURL(ctx context.Context, target string) error
	OpenVNC(ctx context.Context, target string) error
}

type ExecRunner struct{}

type App struct {
	In                        io.Reader
	Out                       io.Writer
	Err                       io.Writer
	Version                   string
	Runner                    Runner
	Validator                 Validator
	StateManager              StateManager
	JobManager                JobManager
	AWSService                AWSService
	WebDir                    string
	MemberStore               MemberRepository
	LogManager                LogManager
	SyncHistory               SyncHistoryStore
	LocalTransfers            *LocalTransferJobManager
	KnownHosts                string
	RemoteUserAPI             bool
	LoginConfigCleanup        bool
	Listen                    func(network, address string) (net.Listener, error)
	WebHandler                http.Handler
	WebReminderWorker         func(context.Context)
	WebAWSLifecycleNotifier   func(event string, reminder ReleaseReminder, operator, description string) error
	WebAWSLifecycleScan       func(context.Context, string) error
	WebShutdownTimeout        time.Duration
	WebWorkerShutdownTimeout  time.Duration
	LocalAgentSecurityCommand func(context.Context, ...string) ([]byte, error)
	LocalAgentServiceCommand  func(context.Context, ...string) ([]byte, error)
	ReadSecret                func(prompt string, in io.Reader, out io.Writer) (string, error)
	TerminalSessions          *terminalSessionRegistry
	LocalAgentBrowserOrigins  map[string]struct{}
}

func NewApp(out, err io.Writer) App {
	return App{
		In:                       os.Stdin,
		Out:                      out,
		Err:                      err,
		Version:                  "dev",
		Runner:                   ExecRunner{},
		Validator:                NewValidator(),
		StateManager:             NewStateManager(DefaultStateDir),
		JobManager:               NewJobManager(DefaultJobDir),
		AWSService:               NewAWSService(),
		MemberStore:              NewMemberStore(DefaultMemberDataPath),
		LogManager:               NewLogManager(DefaultLogDir),
		SyncHistory:              NewSyncHistoryStore(DefaultSyncHistoryPath),
		LocalTransfers:           NewLocalTransferJobManager(),
		ReadSecret:               readInitSecret,
		TerminalSessions:         newTerminalSessionRegistry(256, 30*time.Second),
		KnownHosts:               "~/.ssh/known_hosts",
		LoginConfigCleanup:       true,
		Listen:                   net.Listen,
		WebShutdownTimeout:       5 * time.Second,
		WebWorkerShutdownTimeout: 5 * time.Second,
		LocalAgentSecurityCommand: func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "security", args...).CombinedOutput()
		},
		LocalAgentServiceCommand: func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
		},
	}
}

func (a App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		a.printUsage()
		return 0
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		a.printVersion()
		return 0
	}
	configPath := DefaultConfigPath
	args = parseConfigFlag(args, &configPath)
	if len(args) == 0 {
		a.printUsage()
		return 0
	}
	command := args[0]
	switch command {
	case "init":
		return a.runInit(configPath, args[1:])
	case "skill":
		return a.runSkill(args[1:])
	case "guide":
		return a.runGuide(args[1:])
	case "version":
		a.printVersion()
		return 0
	case "completion":
		return a.runCompletion(ctx, configPath, args[1:])
	case "list":
		cfg, code := a.loadConfig(configPath)
		if code != 0 {
			return code
		}
		return a.runList(ctx, configPath, cfg)
	case "profile":
		cfg, code := a.loadConfig(configPath)
		if code != 0 {
			return code
		}
		return a.runProfile(ctx, configPath, cfg, args[1:])
	case "member":
		return a.runMember(args[1:])
	case "logs":
		return a.runLogs(args[1:])
	case "doctor":
		return a.runDoctor(configPath, args[1:])
	case "dashboard":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runDashboard(ctx, cfg, args[1:])
	case "next":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runNext(ctx, cfg, args[1:])
	case "open":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runAWS(ctx, cfg, append([]string{"open"}, args[1:]...), configPath)
	case "close":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runAWS(ctx, cfg, append([]string{"destroy"}, args[1:]...), configPath)
	case "setup-vnc":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runSetupVNC(cfg, args[1:])
	case "check":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runCheck(cfg, args[1:])
	case "connect":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runConnect(ctx, cfg, args[1:])
	case "ssh":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runSSH(ctx, cfg, args[1:])
	case "exec":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runExec(ctx, cfg, args[1:])
	case "start":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runStart(ctx, cfg, args[1:])
	case "pull":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runPull(ctx, cfg, args[1:])
	case "push":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runPush(ctx, cfg, args[1:])
	case "forget-host":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runForgetHost(ctx, cfg, args[1:])
	case "host-key":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runHostKey(ctx, cfg, args[1:])
	case "open-vnc":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runOpenVNC(ctx, cfg, args[1:])
	case "stop":
		return a.runStop(args[1:])
	case "status":
		return a.runStatus()
	case "job":
		return a.runJob(ctx, args[1:])
	case "web":
		return a.runWeb(ctx, configPath, args[1:])
	case "local-agent":
		return a.runLocalAgent(ctx, configPath, args[1:])
	case "mcp":
		return a.runMCP(ctx, configPath, args[1:])
	case "aws":
		cfg, code := a.loadCommandConfig(ctx, configPath)
		if code != 0 {
			return code
		}
		return a.runAWS(ctx, cfg, args[1:], configPath)
	default:
		fmt.Fprintf(a.Err, "unknown command %q\n\n", command)
		a.printUsage()
		return 2
	}
}
