package connectmac

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func (a App) runAWS(ctx context.Context, cfg Config, args []string, configPath string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "usage: cm aws <plan|capacity|open|create|status|wait-ready|adopt|adopt-host|launch-on-host|destroy|destroy-many|destroy-all|running> [profile-or-apple-email] [--confirm] [--background] [--notify] [--all] [--host-id <id>] [--except <profile-or-apple-email>]")
		return 2
	}
	command := args[0]
	if command == "running" {
		if len(args) != 1 {
			fmt.Fprintln(a.Err, "usage: cm aws running")
			return 2
		}
		return a.runAWSRunning(ctx, cfg)
	}
	if command == "destroy-many" {
		return a.runAWSDestroyMany(ctx, cfg, args[1:])
	}
	if command == "destroy-all" {
		return a.runAWSDestroyAll(ctx, cfg, args[1:])
	}
	if len(args) < 2 {
		fmt.Fprintln(a.Err, "usage: cm aws <plan|capacity|open|create|status|wait-ready|adopt|adopt-host|launch-on-host|destroy> <profile-or-apple-email> [--confirm] [--all] [--host-id <id>]")
		return 2
	}
	profileRef := args[1]
	confirm := false
	hostID := ""
	includeTerminal := false
	background := false
	notify := false
	for i := 2; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--confirm":
			confirm = true
		case "--background":
			background = true
		case "--notify":
			notify = true
		case "--all":
			includeTerminal = true
		case "--host-id":
			i++
			if i >= len(args) || args[i] == "" {
				fmt.Fprintln(a.Err, "--host-id requires a value")
				return 2
			}
			hostID = args[i]
		default:
			fmt.Fprintf(a.Err, "unknown aws option %q\n", arg)
			return 2
		}
	}
	if background && command != "destroy" {
		fmt.Fprintln(a.Err, "--background is currently supported only for cm aws destroy")
		return 2
	}
	if notify && !background {
		fmt.Fprintln(a.Err, "--notify requires --background")
		return 2
	}
	if background && !confirm {
		fmt.Fprintln(a.Err, "--background requires --confirm after preview approval")
		return 2
	}
	run := func() int {
		return a.runAWSProfileCommand(ctx, cfg, command, profileRef, confirm, hostID, includeTerminal, background, notify, configPath)
	}
	if command == "open" || command == "destroy" {
		return a.runObservedAWSCLI(ctx, command, profileRef, confirm, background, run)
	}
	return run()
}

func (a App) runAWSProfileCommand(ctx context.Context, cfg Config, command, profileRef string, confirm bool, hostID string, includeTerminal, background, notify bool, configPath string) int {
	profile, err := resolveProfileRef(cfg, profileRef)
	if err != nil {
		_ = writeCurrentJobOutcome(autoReleaseJobOutcome(TerminalAutoReleaseError(err), false, "profile"))
		fmt.Fprintln(a.Err, err)
		return 2
	}
	if profile.Name != profileRef {
		fmt.Fprintf(a.Out, "Resolved Apple account %s -> profile %s\n", profileRef, profile.Name)
	}
	if (command == "open" || command == "create" || command == "adopt" || command == "adopt-host" || command == "launch-on-host") && confirm {
		var creatorOK bool
		profile, creatorOK = a.promptMissingAWSCreator(profile)
		if !creatorOK {
			fmt.Fprintln(a.Err, "aws.creator is required for confirmed AWS mutations; set aws.creator in the profile or enter it when prompted")
			return 1
		}
	}
	errs := a.Validator.ValidateAWSProfile(profile)
	if len(errs) > 0 {
		_ = writeCurrentJobOutcome(autoReleaseJobOutcome(TerminalAutoReleaseError(errors.Join(errs...)), false, "validation"))
		printErrors(a.Err, errs)
		return 1
	}
	plan, err := a.AWSService.Plan(profile)
	if err != nil {
		_ = writeCurrentJobOutcome(autoReleaseJobOutcome(TerminalAutoReleaseError(err), false, "plan"))
		fmt.Fprintln(a.Err, err)
		return 1
	}
	if background {
		return a.startAWSDestroyJob(ctx, profile, plan, configPath, notify)
	}
	switch command {
	case "plan":
		fmt.Fprint(a.Out, FormatMacPlan(plan))
		return 0
	case "capacity":
		_, capacity, err := a.AWSService.Capacity(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "aws capacity failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSCapacity(plan, capacity))
		return 0
	case "open":
		return a.runAWSOpen(ctx, profile, plan, confirm)
	case "create":
		fmt.Fprint(a.Out, FormatMacPlan(plan))
		if !confirm {
			fmt.Fprintln(a.Out, "Preview only. Run again with --confirm to execute AWS creation.")
			return 0
		}
		_, result, err := a.AWSService.Create(ctx, profile)
		if err != nil {
			fmt.Fprint(a.Err, awsStoppedMessage("aws create", err))
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSCreateResult(plan, result))
		_, status, err := a.awsServiceWithProgress().WaitReady(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "aws wait-ready failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSReadyStatus(plan, status))
		return 0
	case "status":
		_, status, err := a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: includeTerminal})
		if err != nil {
			fmt.Fprintf(a.Err, "aws status failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSStatus(plan, status))
		if !includeTerminal {
			fmt.Fprintln(a.Out, "Terminal resources are hidden. Use --all to include terminated instances and released hosts.")
		}
		return 0
	case "wait-ready":
		_, status, err := a.awsServiceWithProgress().WaitReady(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "aws wait-ready failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSReadyStatus(plan, status))
		return 0
	case "adopt":
		_, status, err := a.AWSService.AdoptionPreview(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "aws adopt failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSAdoptionPreview(plan, status))
		if !confirm {
			fmt.Fprintln(a.Out, "Preview only. Run again with --confirm to tag these resources as cm-managed.")
			return 0
		}
		_, result, err := a.AWSService.Adopt(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "aws adopt failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSAdoptResult(plan, result))
		return 0
	case "adopt-host":
		if hostID == "" {
			fmt.Fprintln(a.Err, "--host-id is required for aws adopt-host")
			return 2
		}
		_, host, err := a.AWSService.AdoptHostPreview(ctx, profile, hostID)
		if err != nil {
			fmt.Fprintf(a.Err, "aws adopt-host failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSAdoptHostPreview(plan, host))
		if !confirm {
			fmt.Fprintln(a.Out, "Preview only. Run again with --confirm to tag this host as cm-managed.")
			return 0
		}
		_, result, err := a.AWSService.AdoptHost(ctx, profile, hostID)
		if err != nil {
			fmt.Fprintf(a.Err, "aws adopt-host failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSAdoptResult(plan, result))
		return 0
	case "launch-on-host":
		if hostID == "" {
			fmt.Fprintln(a.Err, "--host-id is required for aws launch-on-host")
			return 2
		}
		_, preview, err := a.AWSService.LaunchOnHostPreview(ctx, profile, hostID)
		if err != nil {
			fmt.Fprintf(a.Err, "aws launch-on-host failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSLaunchOnHostPreview(plan, preview))
		if !confirm {
			fmt.Fprintln(a.Out, "Preview only. Run again with --confirm to launch EC2 on this host.")
			return 0
		}
		_, result, err := a.AWSService.LaunchOnHost(ctx, profile, hostID)
		if err != nil {
			fmt.Fprint(a.Err, awsStoppedMessage("aws launch-on-host", err))
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSCreateResult(plan, result))
		_, status, err := a.AWSService.WaitReady(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "aws wait-ready failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSReadyStatus(plan, status))
		return 0
	case "destroy":
		return a.runAWSDestroy(ctx, profile, plan, confirm)
	default:
		fmt.Fprintf(a.Err, "unknown aws command %q\n", command)
		return 2
	}
}

func (a App) runObservedAWSCLI(ctx context.Context, command, profileRef string, confirmed, queuedOnSuccess bool, run func() int) int {
	op := operationContextFrom(ctx)
	if op.JobID == "" {
		envOp := operationContextFromJobEnvironment()
		if envOp.JobID != "" {
			op = envOp
		}
	}
	if op.JobID == "" && op.Source != "" && op.Source != "cli" {
		return run()
	}
	if op.RequestID == "" {
		if requestID, err := newRequestID(time.Now(), rand.Reader); err == nil {
			op.RequestID = requestID
		}
	}
	if op.Source == "" {
		op.Source = "cli"
	}
	operation := "aws.open"
	if command == "destroy" {
		operation = "aws.release"
	}
	startedAt := time.Now()
	a.writeRuntimeLog(LogEntry{
		Level:            "info",
		Action:           operation + ".started",
		Operation:        operation,
		Profile:          profileRef,
		RequestID:        op.RequestID,
		JobID:            op.JobID,
		Source:           op.Source,
		Phase:            "started",
		Status:           "running",
		ActorMemberID:    op.Actor.MemberID,
		ActorMemberEmail: op.Actor.MemberEmail,
		ActorMemberName:  op.Actor.MemberName,
		Message:          fmt.Sprintf("AWS CLI %s started; confirmed=%t", command, confirmed),
	})
	code := run()
	phase := "succeeded"
	level := "info"
	errorCode := ""
	status := "success"
	if code == 0 && queuedOnSuccess {
		phase = "queued"
		status = "queued"
	} else if code != 0 {
		phase = "failed"
		level = "error"
		errorCode = "aws_api_error"
		status = "failed"
	}
	a.writeRuntimeLog(LogEntry{
		Level:            level,
		Action:           operation + "." + phase,
		Operation:        operation,
		Profile:          profileRef,
		RequestID:        op.RequestID,
		JobID:            op.JobID,
		Source:           op.Source,
		Phase:            phase,
		ErrorCode:        errorCode,
		Status:           status,
		ElapsedMS:        time.Since(startedAt).Milliseconds(),
		ActorMemberID:    op.Actor.MemberID,
		ActorMemberEmail: op.Actor.MemberEmail,
		ActorMemberName:  op.Actor.MemberName,
		Message:          fmt.Sprintf("AWS CLI %s finished with exit code %d", command, code),
	})
	return code
}

func operationContextFromJobEnvironment() OperationContext {
	return OperationContext{
		RequestID: strings.TrimSpace(os.Getenv(jobRequestIDEnv)),
		JobID:     strings.TrimSpace(os.Getenv(jobIDEnv)),
		Source:    strings.TrimSpace(os.Getenv(jobSourceEnv)),
		Actor: AuditActor{
			MemberID:    strings.TrimSpace(os.Getenv(jobActorMemberIDEnv)),
			MemberEmail: strings.TrimSpace(os.Getenv(jobActorEmailEnv)),
			MemberName:  strings.TrimSpace(os.Getenv(jobActorNameEnv)),
		},
	}
}

func (a App) startAWSDestroyJob(ctx context.Context, profile Profile, plan MacPlan, configPath string, notify bool) int {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(a.Err, "start background destroy failed: %v\n", err)
		return 1
	}
	command := []string{executable, "aws", "destroy", profile.Name, "--confirm", "--config", configPath}
	job, err := a.JobManager.Create(Job{
		Type:       "aws-destroy",
		Profile:    profile.Name,
		AppleEmail: plan.AccountEmail,
		Command:    command,
		Notify:     notify,
	})
	if err != nil {
		fmt.Fprintf(a.Err, "start background destroy failed: %v\n", err)
		return 1
	}
	job, err = a.JobManager.StartRunner(ctx, job)
	if err != nil {
		fmt.Fprintf(a.Err, "start background destroy failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(a.Out, "Started background AWS destroy job: %s\n", job.ID)
	fmt.Fprintf(a.Out, "Profile: %s\n", profile.Name)
	if plan.AccountEmail != "" {
		fmt.Fprintf(a.Out, "Apple account: %s\n", plan.AccountEmail)
	}
	fmt.Fprintf(a.Out, "PID: %d\n", job.PID)
	fmt.Fprintf(a.Out, "Log: %s\n", job.Log)
	fmt.Fprintf(a.Out, "Status: cm job status %s\n", job.ID)
	fmt.Fprintf(a.Out, "Wait: cm job wait %s\n", job.ID)
	fmt.Fprintln(a.Out, "Elastic IP allocation will be retained.")
	return 0
}
func (a App) runAWSDestroy(ctx context.Context, profile Profile, plan MacPlan, confirm bool) int {
	_, status, err := a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: false})
	if err != nil {
		_ = writeCurrentJobOutcome(autoReleaseJobOutcome(err, false, "status"))
		fmt.Fprintf(a.Err, "aws destroy preview failed: %v\n", err)
		return 1
	}
	fmt.Fprint(a.Out, FormatMacDestroyPreviewWithStatus(plan, status))
	if !confirm {
		fmt.Fprintln(a.Out, "Preview only. Run again with --confirm to execute AWS destruction.")
		return 0
	}
	service := a.awsServiceWithProgress()
	_, result, err := service.Destroy(ctx, profile)
	if err != nil {
		outcome := autoReleaseJobOutcome(err, true, "destroy")
		outcome.ReleasedHosts = append([]string(nil), result.ReleasedHosts...)
		_ = writeCurrentJobOutcome(outcome)
		var partial AWSDestroyPartialError
		if errors.As(err, &partial) {
			fmt.Fprint(a.Out, FormatAWSDestroyResult(plan, partial.Result))
			a.printAWSDestroyFinalStatus(ctx, profile)
			fmt.Fprintf(a.Err, "aws destroy partially completed: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.Err, "aws destroy failed: %v\n", err)
		return 1
	}
	fmt.Fprint(a.Out, FormatAWSDestroyResult(plan, result))
	a.printAWSDestroyFinalStatus(ctx, profile)
	if len(result.DeferredHosts) > 0 {
		reason := fmt.Sprintf("AWS destroy deferred for %d dedicated host transition(s)", len(result.DeferredHosts))
		_ = writeCurrentJobOutcome(JobOutcome{ErrorCategory: JobErrorCategoryRecoverable, ErrorCode: "host_transition", Reason: reason, Deferred: true, ReleasedHosts: append([]string(nil), result.ReleasedHosts...)})
	} else {
		_ = writeCurrentJobOutcome(JobOutcome{ReleasedHosts: append([]string(nil), result.ReleasedHosts...)})
	}
	return 0
}
func (a App) printAWSDestroyFinalStatus(ctx context.Context, profile Profile) {
	plan, status, err := a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: false})
	if err != nil {
		fmt.Fprintf(a.Err, "aws destroy final status failed: %v\n", err)
		return
	}
	fmt.Fprint(a.Out, FormatAWSDestroyFinalStatus(plan, status))
}
func (a App) runAWSRunning(ctx context.Context, cfg Config) int {
	rows := [][]string{{"APPLE ACCOUNT", "PROFILE", "REGION", "AZ", "TYPE", "INSTANCE", "PUBLIC IP", "READY"}}
	var errs []string
	for _, name := range sortedProfileNames(cfg) {
		profile, _ := cfg.Profile(name)
		if profile.AWS.Profile == "" {
			continue
		}
		validationErrs := a.Validator.ValidateAWSProfile(profile)
		if len(validationErrs) > 0 {
			continue
		}
		plan, status, err := a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: false})
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", profile.Name, err))
			continue
		}
		for _, instance := range status.Instances {
			if instance.State != "running" {
				continue
			}
			rows = append(rows, []string{
				emptyTableValue(plan.AccountEmail),
				plan.ProfileName,
				plan.Region,
				emptyTableValue(zoneForInstance(status.Hosts, instance.HostID)),
				emptyTableValue(instance.InstanceType),
				emptyTableValue(instance.InstanceID),
				emptyTableValue(instance.PublicIP),
				fmt.Sprintf("%t", InstanceReady(instance, status.ElasticIP)),
			})
		}
	}
	if len(rows) == 1 {
		fmt.Fprintln(a.Out, "No running AWS Mac instances found.")
	} else {
		fmt.Fprint(a.Out, formatRows(rows))
	}
	if len(errs) > 0 {
		fmt.Fprintln(a.Err, "Errors:")
		for _, err := range errs {
			fmt.Fprintf(a.Err, "- %s\n", err)
		}
		return 1
	}
	return 0
}
func zoneForInstance(hosts []DedicatedHostStatus, hostID string) string {
	for _, host := range hosts {
		if host.HostID == hostID {
			return host.ZoneID
		}
	}
	return ""
}
func (a App) runAWSDestroyMany(ctx context.Context, cfg Config, args []string) int {
	refs, confirm, err := parseDestroyManyArgs(args)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 2
	}
	return a.runAWSDestroyProfiles(ctx, cfg, refs, confirm, "destroy-many")
}
func parseDestroyManyArgs(args []string) ([]string, bool, error) {
	confirm := false
	var refs []string
	for _, arg := range args {
		switch arg {
		case "--confirm":
			confirm = true
		default:
			if strings.HasPrefix(arg, "--") {
				return nil, false, fmt.Errorf("unknown destroy-many option %q", arg)
			}
			refs = append(refs, arg)
		}
	}
	if len(refs) == 0 {
		return nil, false, fmt.Errorf("usage: cm aws destroy-many <profile-or-apple-email>... [--confirm]")
	}
	return refs, confirm, nil
}
func (a App) runAWSDestroyAll(ctx context.Context, cfg Config, args []string) int {
	exceptRefs, confirm, err := parseDestroyAllArgs(args)
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 2
	}
	except := map[string]bool{}
	for _, ref := range exceptRefs {
		profile, err := resolveProfileRef(cfg, ref)
		if err != nil {
			fmt.Fprintln(a.Err, err)
			return 2
		}
		except[profile.Name] = true
	}
	var refs []string
	for _, name := range sortedProfileNames(cfg) {
		if !except[name] {
			refs = append(refs, name)
		}
	}
	return a.runAWSDestroyProfiles(ctx, cfg, refs, confirm, "destroy-all")
}
func parseDestroyAllArgs(args []string) ([]string, bool, error) {
	confirm := false
	var exceptRefs []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--confirm":
			confirm = true
		case "--except":
			i++
			if i >= len(args) || args[i] == "" {
				return nil, false, fmt.Errorf("--except requires a profile or Apple email")
			}
			exceptRefs = append(exceptRefs, args[i])
		default:
			return nil, false, fmt.Errorf("unknown destroy-all option %q", args[i])
		}
	}
	return exceptRefs, confirm, nil
}
func (a App) runAWSDestroyProfiles(ctx context.Context, cfg Config, refs []string, confirm bool, command string) int {
	matched := 0
	for _, ref := range refs {
		profile, err := resolveProfileRef(cfg, ref)
		if err != nil {
			fmt.Fprintln(a.Err, err)
			return 2
		}
		if profile.Name != ref && strings.Contains(ref, "@") {
			fmt.Fprintf(a.Out, "Resolved Apple account %s -> profile %s\n", ref, profile.Name)
		}
		errs := a.Validator.ValidateAWSProfile(profile)
		if len(errs) > 0 {
			printErrors(a.Err, errs)
			return 1
		}
		plan, status, err := a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: false})
		if err != nil {
			fmt.Fprintf(a.Err, "aws %s preview failed for %s: %v\n", command, profile.Name, err)
			return 1
		}
		if len(status.Hosts) == 0 && len(status.Instances) == 0 {
			continue
		}
		matched++
		if !confirm {
			fmt.Fprint(a.Out, FormatMacDestroyPreviewWithStatus(plan, status))
			fmt.Fprintln(a.Out, "Preview only. Include --confirm to execute AWS destruction.")
			continue
		}
		code := a.runAWSDestroy(ctx, profile, plan, true)
		if code != 0 {
			return code
		}
	}
	if matched == 0 {
		fmt.Fprintf(a.Out, "No active AWS Mac resources matched %s.\n", command)
	}
	return 0
}
func (a App) runAWSOpen(ctx context.Context, profile Profile, plan MacPlan, confirm bool) int {
	_, status, err := a.AWSService.Status(ctx, profile)
	if err != nil {
		fmt.Fprintf(a.Err, "aws open failed: %v\n", err)
		return 1
	}
	action := AWSOpenAction(status)
	var candidates []AWSCreateAttempt
	if action.Kind == "create" {
		_, values, err := a.AWSService.CreateCandidates(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "aws open failed: %v\n", err)
			return 1
		}
		candidates = values
	}
	fmt.Fprint(a.Out, FormatAWSOpenPreviewWithCandidates(plan, status, candidates))
	if !confirm {
		fmt.Fprintln(a.Out, "Preview only. Run again with --confirm to open or wait for this Mac.")
		return 0
	}
	switch action.Kind {
	case "ready":
		fmt.Fprint(a.Out, FormatAWSReadyStatus(plan, status))
		return 0
	case "wait-ready":
		_, readyStatus, err := a.awsServiceWithProgress().WaitReady(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "aws wait-ready failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSReadyStatus(plan, readyStatus))
		return 0
	case "launch-on-host":
		_, result, err := a.AWSService.LaunchOnHost(ctx, profile, action.HostID)
		if err != nil {
			fmt.Fprint(a.Err, awsStoppedMessage("aws launch-on-host", err))
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSCreateResult(plan, result))
		_, readyStatus, err := a.awsServiceWithProgress().WaitReady(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "aws wait-ready failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSReadyStatus(plan, readyStatus))
		return 0
	case "create":
		_, result, err := a.AWSService.Create(ctx, profile)
		if err != nil {
			fmt.Fprint(a.Err, awsStoppedMessage("aws create", err))
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSCreateResult(plan, result))
		_, readyStatus, err := a.awsServiceWithProgress().WaitReady(ctx, profile)
		if err != nil {
			fmt.Fprintf(a.Err, "aws wait-ready failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatAWSReadyStatus(plan, readyStatus))
		return 0
	default:
		fmt.Fprintf(a.Err, "aws open cannot continue automatically: %s\n", action.Detail)
		return 1
	}
}
func (a App) awsServiceWithProgress() AWSService {
	service := a.AWSService
	service.Progress = func(message string) {
		fmt.Fprintln(a.Out, message)
	}
	return service
}
func awsStoppedMessage(operation string, err error) string {
	return fmt.Sprintf("%s failed: %v\nStopped. Report this reason to the user and wait for explicit instructions before continuing.\n", operation, err)
}
