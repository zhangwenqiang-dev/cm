package connectmac

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	webAWSLifecycleNotificationClaimLease  = 5 * time.Minute
	webAWSLifecycleNotificationMaxAttempts = 5
)

var webAWSLifecycleNotificationBackoff = [...]time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
}

func awsLifecycleOpenReady(status AWSStatus) bool {
	return AWSStatusReady(status)
}

func awsLifecycleStopped(status AWSStatus) bool {
	return len(status.Hosts) == 0 &&
		len(status.Instances) == 0 &&
		strings.TrimSpace(status.ElasticIP.InstanceID) == ""
}

func (a App) reconcileWebAWSLifecycles(ctx context.Context, configPath string) error {
	jobs, err := a.JobManager.List()
	if err != nil {
		return err
	}
	var reconcileErrors []error
	for _, job := range jobs {
		if !webAWSLifecycleJobParticipates(job) {
			continue
		}
		if err := a.reconcileWebAWSLifecycleJob(ctx, configPath, job); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("job %s: %w", job.ID, err))
		}
	}
	return errors.Join(reconcileErrors...)
}

func webAWSLifecycleJobParticipates(job Job) bool {
	if job.LifecycleState == "" {
		return false
	}
	if job.Type != "aws-open" && job.Type != "aws-destroy" {
		return false
	}
	if job.LifecycleState == JobLifecycleFailed {
		return job.LifecycleEventRecordedAt.IsZero()
	}
	if job.LifecycleState != JobLifecycleFinalized {
		return true
	}
	if !job.LifecycleNotifiedAt.IsZero() {
		return false
	}
	return true
}

func (a App) reconcileWebAWSLifecycleJob(ctx context.Context, configPath string, job Job) error {
	profileName := strings.TrimSpace(job.Profile)
	if profileName == "" {
		return a.recordWebAWSLifecycleRetry(job.ID, errors.New("lifecycle profile is required"))
	}
	return a.JobManager.WithProfileOperation(profileName, func() error {
		current, err := a.JobManager.Load(job.ID)
		if err != nil {
			return err
		}
		if !webAWSLifecycleJobParticipates(current) {
			return nil
		}
		stale, err := a.hasNewerWebAWSLifecycleJob(current)
		if err != nil {
			return err
		}
		if stale {
			return nil
		}
		return a.reconcileWebAWSLifecycleJobLocked(ctx, configPath, current)
	})
}

func (a App) reconcileWebAWSLifecycleJobLocked(ctx context.Context, configPath string, job Job) error {
	if !webAWSLifecycleJobParticipates(job) {
		return nil
	}
	if job.Status == JobStatusFailed || job.Status == JobStatusInterrupted {
		if job.LifecycleEventRecordedAt.IsZero() {
			if err := a.recordWebAWSLifecycleTerminal(job, false); err != nil {
				return err
			}
		}
		_, err := a.JobManager.Update(job.ID, func(current Job) (Job, error) {
			if current.LifecycleState == JobLifecycleFinalized {
				return current, nil
			}
			current.LifecycleState = JobLifecycleFailed
			current.LifecycleError = strings.TrimSpace(current.LastError)
			if current.LifecycleError == "" {
				current.LifecycleError = string(current.Status)
			}
			if current.LifecycleEventRecordedAt.IsZero() {
				current.LifecycleEventRecordedAt = time.Now()
			}
			return current, nil
		})
		return err
	}
	if job.Type == "aws-open" && job.Status != JobStatusSuccess {
		return nil
	}
	if job.Type == "aws-destroy" && job.Status != JobStatusSuccess && job.Status != JobStatusDeferred {
		return nil
	}

	profile, err := a.resolveWebAWSLifecycleProfile(configPath, job)
	if err != nil {
		return a.recordWebAWSLifecycleRetry(job.ID, err)
	}
	current, err := a.JobManager.Load(job.ID)
	if err != nil {
		return err
	}
	if current.LifecycleState == JobLifecycleFinalized {
		if current.LifecycleEventRecordedAt.IsZero() {
			if err := a.recordWebAWSLifecycleTerminal(current, true); err != nil {
				return err
			}
			current, err = a.JobManager.Update(job.ID, func(latest Job) (Job, error) {
				if latest.LifecycleEventRecordedAt.IsZero() {
					latest.LifecycleEventRecordedAt = time.Now()
				}
				return latest, nil
			})
			if err != nil {
				return err
			}
		}
		return a.notifyFinalizedWebAWSLifecycle(current, profile, ReleaseReminder{})
	}
	_, status, err := a.AWSService.StatusWithOptions(ctx, profile, AWSStatusOptions{IncludeTerminal: false})
	if err != nil {
		return a.recordWebAWSLifecycleRetry(job.ID, fmt.Errorf("aws status failed: %w", err))
	}

	ready := job.Type == "aws-open" && awsLifecycleOpenReady(status)
	stopped := job.Type == "aws-destroy" && awsLifecycleStopped(status)
	if !ready && !stopped {
		_, err := a.JobManager.Update(job.ID, func(current Job) (Job, error) {
			if current.LifecycleState != JobLifecycleFinalized {
				current.LifecycleState = JobLifecycleWaiting
				current.LifecycleError = ""
			}
			return current, nil
		})
		return err
	}

	var reminder ReleaseReminder
	if current.LifecycleState != JobLifecycleFinalized {
		switch current.Type {
		case "aws-open":
			reminder, err = a.finalizeWebAWSOpen(profile, current.LifecycleOwnerEmail, status)
		case "aws-destroy":
			reminder, err = a.finalizeWebAWSDestroy(profile)
		}
		if err != nil {
			return a.recordWebAWSLifecycleRetry(job.ID, err)
		}
		if err := a.recordWebAWSLifecycleTerminal(current, true); err != nil {
			return err
		}
		current, err = a.JobManager.Update(job.ID, func(latest Job) (Job, error) {
			if latest.LifecycleState != JobLifecycleFinalized {
				latest.LifecycleState = JobLifecycleFinalized
				latest.LifecycleFinalizedAt = time.Now()
				latest.LifecycleEventRecordedAt = latest.LifecycleFinalizedAt
				latest.LifecycleError = ""
			}
			return latest, nil
		})
		if err != nil {
			return err
		}
	}
	return a.notifyFinalizedWebAWSLifecycle(current, profile, reminder)
}

func (a App) recordWebAWSLifecycleTerminal(job Job, success bool) error {
	actionPrefix := "aws.open"
	phase := "ready"
	if job.Type == "aws-destroy" {
		actionPrefix = "aws.release"
		phase = "stopped"
	}
	status := "success"
	errorCode := ""
	message := ""
	if !success {
		phase = "failed"
		status = "failed"
		message = strings.TrimSpace(job.LastError)
		if message == "" {
			message = string(job.Status)
		}
		errorCode = strings.TrimSpace(job.ErrorCode)
		if errorCode == "" {
			errorCode = classifyOperationalError(errors.New(message)).Code
		}
	}
	duration := time.Since(job.StartedAt)
	if duration < 0 {
		duration = 0
	}
	if success && job.Type == "aws-destroy" {
		message = "eip_retained=true"
	}
	return a.MemberStore.RecordEvent(OperationEvent{
		ID:          "event-" + job.ID + "-" + phase,
		Action:      actionPrefix + "." + phase,
		Profile:     job.Profile,
		AppleEmail:  job.AppleEmail,
		MemberID:    job.ActorMemberID,
		MemberEmail: job.ActorEmail,
		MemberName:  job.ActorName,
		RequestID:   job.RequestID,
		JobID:       job.ID,
		Source:      job.Source,
		Phase:       phase,
		ErrorCode:   errorCode,
		DurationMS:  duration.Milliseconds(),
		Confirmed:   true,
		Status:      status,
		Message:     message,
	})
}

func (a App) hasNewerWebAWSLifecycleJob(current Job) (bool, error) {
	jobs, err := a.JobManager.List()
	if err != nil {
		return false, err
	}
	currentEmail := normalizeEmail(current.AppleEmail)
	for _, candidate := range jobs {
		if candidate.ID == current.ID ||
			(candidate.Type != "aws-open" && candidate.Type != "aws-destroy") ||
			candidate.Profile != current.Profile ||
			normalizeEmail(candidate.AppleEmail) != currentEmail {
			continue
		}
		if candidate.StartedAt.After(current.StartedAt) ||
			(candidate.StartedAt.Equal(current.StartedAt) && candidate.ID > current.ID) {
			return true, nil
		}
	}
	return false, nil
}

func (a App) notifyFinalizedWebAWSLifecycle(job Job, profile Profile, reminder ReleaseReminder) error {
	if !job.LifecycleNotifiedAt.IsZero() {
		return nil
	}
	var err error
	if reminder.ProfileName == "" {
		var ok bool
		reminder, ok, err = a.MemberStore.ReleaseReminder(profile.Name)
		if err != nil {
			return err
		}
		if !ok {
			reminder = ReleaseReminder{
				ProfileName: profile.Name,
				AppleEmail:  profile.AWS.AccountEmail,
				Status:      ReleaseReminderStatusReleased,
			}
		}
	}
	event, description, operator := "release", "Mac 释放成功", ""
	if job.Type == "aws-open" {
		event, description = "open", "Mac 打开确认成功"
		operator = reminder.OwnerName
	}
	now := a.JobManager.normalize().Now()
	fingerprint := wechatWebhookConfigFingerprint(os.Getenv(envWechatWebhookURL))
	if job.LifecycleNotifyConfigFingerprint == "" {
		job, err = a.JobManager.Update(job.ID, func(latest Job) (Job, error) {
			if !latest.LifecycleNotifiedAt.IsZero() {
				return latest, nil
			}
			latest.LifecycleNotifyConfigFingerprint = fingerprint
			return latest, nil
		})
		if err != nil {
			return err
		}
	} else if job.LifecycleNotifyConfigFingerprint != fingerprint && !job.LifecycleNotifyExhaustedAt.IsZero() {
		job, err = a.JobManager.Update(job.ID, func(latest Job) (Job, error) {
			if !latest.LifecycleNotifiedAt.IsZero() || latest.LifecycleNotifyExhaustedAt.IsZero() {
				return latest, nil
			}
			latest.LifecycleNotifyConfigFingerprint = fingerprint
			latest.LifecycleNotifyAttempts = 0
			latest.LifecycleNotifyNextAttemptAt = time.Time{}
			latest.LifecycleNotifyExhaustedAt = time.Time{}
			latest.LifecycleNotifyFailureRecordedAt = time.Time{}
			latest.LifecycleNotifyClaimedAt = time.Time{}
			latest.LifecycleError = ""
			return latest, nil
		})
		if err != nil {
			return err
		}
	}
	if !job.LifecycleNotifyNextAttemptAt.IsZero() && now.Before(job.LifecycleNotifyNextAttemptAt) {
		return nil
	}
	if !job.LifecycleNotifyExhaustedAt.IsZero() {
		return a.ensureWebAWSLifecycleNotificationFailureRecorded(job, event, WechatNotifyResult{}, errors.New(job.LifecycleError))
	}
	didClaim := false
	claimed, err := a.JobManager.Update(job.ID, func(latest Job) (Job, error) {
		if !latest.LifecycleNotifiedAt.IsZero() || !latest.LifecycleNotifyExhaustedAt.IsZero() {
			return latest, nil
		}
		if !latest.LifecycleNotifyClaimedAt.IsZero() &&
			now.Sub(latest.LifecycleNotifyClaimedAt) < webAWSLifecycleNotificationClaimLease {
			return latest, nil
		}
		if latest.LifecycleNotifyAttempts >= webAWSLifecycleNotificationMaxAttempts {
			latest.LifecycleNotifyExhaustedAt = now
			latest.LifecycleNotifyClaimedAt = time.Time{}
			return latest, nil
		}
		latest.LifecycleNotifyClaimedAt = now
		latest.LifecycleNotifyAttempts++
		latest.LifecycleNotifyNextAttemptAt = time.Time{}
		latest.LifecycleError = ""
		didClaim = true
		return latest, nil
	})
	if err != nil {
		return err
	}
	if !didClaim {
		if !claimed.LifecycleNotifyExhaustedAt.IsZero() {
			return a.ensureWebAWSLifecycleNotificationFailureRecorded(claimed, event, WechatNotifyResult{}, errors.New(claimed.LifecycleError))
		}
		return nil
	}
	deliveryContext, result, notifyErr := a.notifyWebAWSLifecycleObserved(claimed, event, reminder, operator, description)
	if notifyErr != nil {
		cause := errors.New(redactWechatWebhookURL(fmt.Sprintf("notify lifecycle success: %v", notifyErr)))
		configurationFailure := result.Skipped || errors.Is(notifyErr, errWechatWebhookNotConfigured)
		exhausted := configurationFailure || claimed.LifecycleNotifyAttempts >= webAWSLifecycleNotificationMaxAttempts
		updated, clearErr := a.JobManager.Update(job.ID, func(latest Job) (Job, error) {
			if latest.LifecycleNotifiedAt.IsZero() && latest.LifecycleNotifyClaimedAt.Equal(claimed.LifecycleNotifyClaimedAt) {
				latest.LifecycleNotifyClaimedAt = time.Time{}
				latest.LifecycleError = cause.Error()
				if exhausted {
					latest.LifecycleNotifyExhaustedAt = now
					latest.LifecycleNotifyNextAttemptAt = time.Time{}
				} else {
					latest.LifecycleNotifyNextAttemptAt = now.Add(
						webAWSLifecycleNotificationBackoff[claimed.LifecycleNotifyAttempts-1],
					)
				}
			}
			return latest, nil
		})
		if clearErr != nil {
			return errors.Join(cause, clearErr)
		}
		if exhausted {
			recordErr := a.ensureWebAWSLifecycleNotificationFailureRecorded(updated, event, result, cause)
			return errors.Join(cause, recordErr)
		}
		retryErr := a.recordWechatDeliveryState(deliveryContext, "retrying", result, cause)
		return errors.Join(cause, retryErr)
	}
	if err := a.recordWechatDeliveryState(deliveryContext, "sent", result, nil); err != nil {
		return err
	}
	_, err = a.JobManager.Update(job.ID, func(latest Job) (Job, error) {
		if latest.LifecycleNotifiedAt.IsZero() && latest.LifecycleNotifyClaimedAt.Equal(claimed.LifecycleNotifyClaimedAt) {
			latest.LifecycleNotifiedAt = now
			latest.LifecycleNotifyClaimedAt = time.Time{}
			latest.LifecycleNotifyNextAttemptAt = time.Time{}
			latest.LifecycleError = ""
		}
		return latest, nil
	})
	return err
}

func (a App) recordWebAWSLifecycleRetry(jobID string, cause error) error {
	_, updateErr := a.JobManager.Update(jobID, func(current Job) (Job, error) {
		if current.LifecycleState != JobLifecycleFinalized {
			current.LifecycleState = JobLifecycleWaiting
		}
		current.LifecycleError = cause.Error()
		return current, nil
	})
	return errors.Join(cause, updateErr)
}

func (a App) resolveWebAWSLifecycleProfile(configPath string, job Job) (Profile, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) || !os.IsNotExist(pathErr.Err) {
			return Profile{}, err
		}
		cfg = Config{Profiles: map[string]Profile{}}
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	records, err := a.MemberStore.ListManagedProfiles("")
	if err != nil {
		return Profile{}, fmt.Errorf("load managed profiles: %w", err)
	}
	var matched Profile
	matchedOK := false
	if local, ok := cfg.Profile(strings.TrimSpace(job.Profile)); ok &&
		normalizeEmail(local.AWS.AccountEmail) == normalizeEmail(job.AppleEmail) {
		matched = local
		matchedOK = true
	}
	for _, record := range records {
		profile, parseErr := ParseSingleProfileYAML(record.ProfileYAML)
		if parseErr != nil {
			return Profile{}, fmt.Errorf("parse managed profile %s: %w", record.Name, parseErr)
		}
		if profile.Name != strings.TrimSpace(job.Profile) ||
			normalizeEmail(profile.AWS.AccountEmail) != normalizeEmail(job.AppleEmail) {
			continue
		}
		if local, ok := cfg.Profile(profile.Name); ok {
			applyLocalPrivateProfileFields(&profile, local)
		}
		matched = profile
		matchedOK = true
	}
	if !matchedOK {
		return Profile{}, fmt.Errorf("lifecycle profile %q with Apple account %q not found", job.Profile, job.AppleEmail)
	}
	cfg.Profiles = map[string]Profile{matched.Name: matched}
	cfg.ApplyDefaults()
	profile, _ := cfg.Profile(matched.Name)
	jobAppleEmail := normalizeEmail(job.AppleEmail)
	profileAppleEmail := normalizeEmail(profile.AWS.AccountEmail)
	if jobAppleEmail == "" || profileAppleEmail != jobAppleEmail {
		return Profile{}, fmt.Errorf("lifecycle profile %q Apple account %q does not match job Apple account %q", profile.Name, profile.AWS.AccountEmail, job.AppleEmail)
	}
	if errs := a.Validator.ValidateAWSProfile(profile); len(errs) > 0 {
		return Profile{}, errors.New(strings.Join(validationMessages(errs), "\n"))
	}
	return profile, nil
}

func (a App) finalizeWebAWSOpen(profile Profile, ownerEmail string, status AWSStatus) (ReleaseReminder, error) {
	ownerEmail = normalizeEmail(ownerEmail)
	if ownerEmail == "" {
		return ReleaseReminder{}, errors.New("lifecycle owner email is required")
	}
	if _, err := a.MemberStore.AssignMember(profile.AWS.AccountEmail, ownerEmail, "owner"); err != nil {
		return ReleaseReminder{}, err
	}
	owner, err := a.MemberStore.SetProfileOwner(profile.Name, ownerEmail)
	if err != nil {
		return ReleaseReminder{}, err
	}
	now := time.Now()
	hostID := ""
	hostArchitecture := ""
	hostCreatedAt := now.Format(time.RFC3339)
	for _, host := range status.Hosts {
		if host.HostID == "" || strings.EqualFold(host.State, "released") {
			continue
		}
		hostID = host.HostID
		hostArchitecture = MacHostArchitecture(host.InstanceType)
		if strings.TrimSpace(host.CreatedAt) != "" {
			hostCreatedAt = host.CreatedAt
		}
		break
	}
	existing, ok, err := a.MemberStore.ReleaseReminder(profile.Name)
	if err != nil {
		return ReleaseReminder{}, err
	}
	if ok && existing.HostID == hostID && existing.Status != ReleaseReminderStatusReleased {
		existing.OwnerEmail = owner.Owner.Email
		existing.OwnerName = owner.Owner.Name
		existing.HostArchitecture = hostArchitecture
		return a.MemberStore.UpsertReleaseReminder(existing)
	}
	createdAt, parseErr := time.Parse(time.RFC3339, hostCreatedAt)
	if parseErr != nil {
		createdAt = now
		hostCreatedAt = now.Format(time.RFC3339)
	}
	return a.MemberStore.UpsertReleaseReminder(ReleaseReminder{
		ProfileName:      profile.Name,
		AppleEmail:       profile.AWS.AccountEmail,
		HostID:           hostID,
		HostArchitecture: hostArchitecture,
		HostCreatedAt:    hostCreatedAt,
		ReleaseDueAt:     createdAt.Add(24 * time.Hour).Format(time.RFC3339),
		OwnerEmail:       owner.Owner.Email,
		OwnerName:        owner.Owner.Name,
		Status:           ReleaseReminderStatusActive,
	})
}

func (a App) finalizeWebAWSDestroy(profile Profile) (ReleaseReminder, error) {
	if err := a.MemberStore.ClearProfileOwner(profile.Name); err != nil {
		return ReleaseReminder{}, err
	}
	reminder, ok, err := a.MemberStore.ReleaseReminder(profile.Name)
	if err != nil {
		return ReleaseReminder{}, err
	}
	if !ok {
		return ReleaseReminder{ProfileName: profile.Name, AppleEmail: profile.AWS.AccountEmail, Status: ReleaseReminderStatusReleased}, nil
	}
	if reminder.Status == ReleaseReminderStatusReleased {
		return reminder, nil
	}
	return a.MemberStore.MarkReleaseReminderReleased(profile.Name, time.Now().Format(time.RFC3339))
}

func (a App) notifyWebAWSLifecycle(event string, reminder ReleaseReminder, operator, description string) error {
	if a.WebAWSLifecycleNotifier != nil {
		return a.WebAWSLifecycleNotifier(event, reminder, operator, description)
	}
	return a.notifyReleaseReminder(event, reminder, operator, description)
}

func (a App) notifyWebAWSLifecycleObserved(job Job, event string, reminder ReleaseReminder, operator, description string) (wechatDeliveryContext, WechatNotifyResult, error) {
	deliveryContext := wechatDeliveryContext{
		RequestID:   job.RequestID,
		JobID:       job.ID,
		Profile:     job.Profile,
		AppleEmail:  job.AppleEmail,
		Source:      job.Source,
		Event:       event,
		DeliveryKey: job.LifecycleNotifyConfigFingerprint,
		Actor: AuditActor{
			MemberID:    job.ActorMemberID,
			MemberEmail: job.ActorEmail,
			MemberName:  job.ActorName,
		},
		Attempt: job.LifecycleNotifyAttempts,
	}
	notification := WechatNotification{
		Event:            event,
		AppleEmail:       reminder.AppleEmail,
		Owner:            displayNameEmail(reminder.OwnerName, reminder.OwnerEmail),
		Operator:         operator,
		HostID:           reminder.HostID,
		HostArchitecture: reminder.HostArchitecture,
		HostCreatedAt:    reminder.HostCreatedAt,
		DueAt:            reminder.ReleaseDueAt,
		Management:       true,
		Description:      description,
	}
	send := func() (WechatNotifyResult, error) {
		if a.WebAWSLifecycleNotifier != nil {
			err := a.WebAWSLifecycleNotifier(event, reminder, operator, description)
			return WechatNotifyResult{}, err
		}
		return NewWechatNotifierFromEnv().Send(notification)
	}
	// WeChat exposes no idempotency key. Persist pending immediately before the
	// HTTP call and sent immediately after it to keep the unavoidable at-least-once window minimal.
	result, err := a.attemptWechatNotification(deliveryContext, send)
	return deliveryContext, result, err
}

func (a App) ensureWebAWSLifecycleNotificationFailureRecorded(job Job, event string, result WechatNotifyResult, cause error) error {
	if !job.LifecycleNotifyFailureRecordedAt.IsZero() {
		return nil
	}
	deliveryContext := wechatDeliveryContext{
		RequestID:   job.RequestID,
		JobID:       job.ID,
		Profile:     job.Profile,
		AppleEmail:  job.AppleEmail,
		Source:      job.Source,
		Event:       event,
		DeliveryKey: job.LifecycleNotifyConfigFingerprint,
		Actor: AuditActor{
			MemberID:    job.ActorMemberID,
			MemberEmail: job.ActorEmail,
			MemberName:  job.ActorName,
		},
		Attempt: job.LifecycleNotifyAttempts,
	}
	if cause == nil || strings.TrimSpace(cause.Error()) == "" {
		cause = errors.New("notification attempts exhausted")
	}
	if err := a.recordWechatDeliveryState(deliveryContext, "failed", result, cause); err != nil {
		return err
	}
	now := a.JobManager.normalize().Now()
	_, err := a.JobManager.Update(job.ID, func(latest Job) (Job, error) {
		if latest.LifecycleNotifyFailureRecordedAt.IsZero() {
			latest.LifecycleNotifyFailureRecordedAt = now
		}
		return latest, nil
	})
	return err
}
