package connectmac

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (a App) runInit(ctx context.Context, configPath string, args []string) int {
	if len(args) > 1 || (len(args) == 1 && args[0] != "wizard") {
		option := ""
		if len(args) > 0 {
			option = args[0]
		}
		fmt.Fprintf(a.Err, "unknown init option %q\n", option)
		return 2
	}
	return a.runGuidedInit(ctx, configPath)
}

func (a App) runGuidedInit(ctx context.Context, configPath string) int {
	a.In = persistentInitInput(a.In)
	path, err := ExpandPath(configPath)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}

	original, err := os.ReadFile(path)
	firstRun := os.IsNotExist(err)
	if err != nil && !firstRun {
		fmt.Fprintf(a.Err, "read config: %v\n", err)
		return 1
	}
	if firstRun {
		original = nil
	}

	doc, err := newInitConfigDocument(original)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	if strings.TrimSpace(doc.ServerUserAPI()) == "" {
		doc.SetServerUserAPI(DefaultConnectMacServer)
	}
	if strings.TrimSpace(doc.DefaultUser()) == "" {
		doc.SetDefaultUser(DefaultAWSUser)
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}

	pemState := "configured"
	if strings.TrimSpace(doc.DefaultIdentityFile()) == "" {
		selected, state, err := a.chooseInitPEM(ctx)
		if err != nil {
			fmt.Fprintln(a.Err, err)
			return 1
		}
		pemState = state
		if selected != "" {
			doc.SetDefaultIdentityFile(selected)
		}
	}

	if strings.TrimSpace(doc.ServerToken()) == "" {
		if err := ctx.Err(); err != nil {
			fmt.Fprintln(a.Err, err)
			return 1
		}
		token, err := a.promptInitToken(ctx, doc.ServerUserAPI())
		if err != nil {
			fmt.Fprintln(a.Err, err)
			return 1
		}
		if token != "" {
			doc.SetServerToken(token)
		}
	}

	if err := ctx.Err(); err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	data, changed, err := doc.Bytes()
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	result := "unchanged"
	if changed {
		if err := ctx.Err(); err != nil {
			fmt.Fprintln(a.Err, err)
			return 1
		}
		if err := writePrivateFileAtomic(path, data); err != nil {
			fmt.Fprintf(a.Err, "write config: %v\n", err)
			return 1
		}
		if firstRun {
			result = "created"
		} else {
			result = "updated"
		}
	}

	a.printInitSummary(path, result, doc, pemState)
	if err := ctx.Err(); err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	if strings.EqualFold(a.promptLine("Initialize AI Skill now? [y/N]: "), "y") {
		return a.runSkillSetup(nil)
	}
	return 0
}

func (a App) chooseInitPEM(ctx context.Context) (string, string, error) {
	userHomeDir := a.UserHomeDir
	if userHomeDir == nil {
		userHomeDir = os.UserHomeDir
	}
	home, err := userHomeDir()
	if err != nil {
		fmt.Fprintf(a.Err, "warning: PEM discovery failed: find home directory: %v\n", err)
		return "", "missing", nil
	}
	discover := a.DiscoverInitPEMFiles
	if discover == nil {
		discover = discoverInitPEMFiles
	}
	files, err := discover(filepath.Join(home, ".ssh"))
	if err != nil {
		fmt.Fprintf(a.Err, "warning: PEM discovery failed: %v\n", err)
		return "", "missing", nil
	}
	if len(files) == 0 {
		fmt.Fprintln(a.Out, "No PEM files found directly under ~/.ssh.")
		return "", "missing", nil
	}

	fmt.Fprintln(a.Out, "Available PEM files:")
	for i, file := range files {
		fmt.Fprintf(a.Out, "  %d. %s\n", i+1, file)
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", "skipped", err
		}
		choice := a.promptLine("Select default PEM (0 to skip): ")
		if choice == "" || choice == "0" {
			return "", "skipped", nil
		}
		index, err := strconv.Atoi(choice)
		if err == nil && index >= 1 && index <= len(files) {
			return files[index-1], "configured", nil
		}
		fmt.Fprintf(a.Err, "choose a number from 1 to %d, or 0 to skip\n", len(files))
	}
}

func (a App) promptInitToken(ctx context.Context, serverURL string) (string, error) {
	if a.ReadSecret == nil {
		return "", fmt.Errorf("read token: secret reader is not configured")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fmt.Fprintf(a.Err, "Token validation server: %s\n", serverURL)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		token, err := a.ReadSecret("ConnectMac API token (empty to skip): ", a.In, a.Err)
		if err != nil {
			return "", err
		}
		if token == "" {
			return "", nil
		}
		if err := a.validateInitToken(ctx, serverURL, token); err == nil {
			return token, nil
		} else {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", err
			}
			fmt.Fprintf(a.Err, "Token validation failed: %v\n", redactInitTokenError(err, token))
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !strings.EqualFold(a.promptLine("Retry token entry? [y/N]: "), "y") {
			return "", nil
		}
	}
}

func (a App) printInitSummary(path, result string, doc *initConfigDocument, pemState string) {
	fmt.Fprintf(a.Out, "Config %s: %s\n", result, path)
	fmt.Fprintf(a.Out, "Server: %s\n", doc.ServerUserAPI())
	fmt.Fprintf(a.Out, "SSH user: %s\n", doc.DefaultUser())
	if strings.TrimSpace(doc.ServerToken()) == "" {
		fmt.Fprintln(a.Out, "Token: skipped")
	} else {
		fmt.Fprintln(a.Out, "Token: configured")
	}
	identity := strings.TrimSpace(doc.DefaultIdentityFile())
	if identity == "" {
		fmt.Fprintf(a.Out, "PEM: %s\n", pemState)
	} else if err := initPEMReadability(identity); err != nil {
		fmt.Fprintf(a.Out, "PEM: configured (%s, not readable: %v)\n", identity, err)
	} else {
		fmt.Fprintf(a.Out, "PEM: configured (%s, readable)\n", identity)
	}
	if strings.TrimSpace(doc.ServerToken()) == "" {
		fmt.Fprintf(a.Out, "Next: generate a token in the management page at %s, then rerun cm init.\n", doc.ServerUserAPI())
	} else if identity == "" {
		fmt.Fprintln(a.Out, "Next: rerun cm init to configure skipped items.")
	} else {
		fmt.Fprintln(a.Out, "Next: run cm list.")
	}
}

func initPEMReadability(path string) error {
	expanded, err := ExpandPath(path)
	if err != nil {
		return err
	}
	file, err := os.Open(expanded)
	if err != nil {
		return err
	}
	return file.Close()
}

func DefaultConfigTemplate() string {
	return "server:\n  user_api: https://cm.hsgitlab.xyz\ndefaults:\n  user: ec2-user\n"
}
