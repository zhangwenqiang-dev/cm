package connectmac

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	AutoReleaseGracePeriod   = 10 * time.Minute
	AutoReleaseRetryInterval = 5 * time.Minute
	AutoReleaseRetryWindow   = time.Hour
)

type AutoReleaseNotificationKind string

const (
	AutoReleaseNotificationDue          AutoReleaseNotificationKind = "due"
	AutoReleaseNotificationFirstFailure AutoReleaseNotificationKind = "first_failure"
	AutoReleaseNotificationFinalFailure AutoReleaseNotificationKind = "final_failure"
	AutoReleaseNotificationSuccess      AutoReleaseNotificationKind = "success"
)

type AutoReleaseNotification struct {
	Kind     AutoReleaseNotificationKind
	Reminder ReleaseReminder
	Error    string
}

type AutoReleaseEvent struct {
	Action   string
	Reminder ReleaseReminder
	Attempt  int
	Message  string
}

type AutoReleaseStore interface {
	ListReleaseReminders(memberEmail string) ([]ReleaseReminder, error)
	ReleaseReminder(profileName string) (ReleaseReminder, bool, error)
	UpdateReleaseReminder(profileName string, update func(ReleaseReminder) (ReleaseReminder, error)) (ReleaseReminder, error)
	MarkAutoReleaseNotified(cycle ReleaseReminderCycle, notifiedAt string) (ReleaseReminder, error)
	CompleteAutoRelease(cycle ReleaseReminderCycle, releasedAt string) (ReleaseReminder, error)
}

type AutoReleaseJobs interface {
	Active() ([]Job, error)
	List() ([]Job, error)
}

type AutoReleaseCoordinator struct {
	Now            func() time.Time
	Store          AutoReleaseStore
	Jobs           AutoReleaseJobs
	ResolveProfile func(context.Context, ReleaseReminder) (Profile, error)
	Status         func(context.Context, Profile) (AWSStatus, error)
	StartDestroy   func(context.Context, Profile) (Job, error)
	Notify         func(AutoReleaseNotification) error
	Emit           func(AutoReleaseEvent)
}

type autoReleaseErrorCategory uint8

const (
	autoReleaseErrorUnknown autoReleaseErrorCategory = iota
	autoReleaseErrorRecoverable
	autoReleaseErrorTerminal
)

type categorizedAutoReleaseError struct {
	category autoReleaseErrorCategory
	cause    error
}

func (e categorizedAutoReleaseError) Error() string { return e.cause.Error() }
func (e categorizedAutoReleaseError) Unwrap() error { return e.cause }

func TerminalAutoReleaseError(err error) error {
	if err == nil {
		return nil
	}
	return categorizedAutoReleaseError{category: autoReleaseErrorTerminal, cause: err}
}

func RecoverableAutoReleaseError(err error) error {
	if err == nil {
		return nil
	}
	return categorizedAutoReleaseError{category: autoReleaseErrorRecoverable, cause: err}
}

func applyReleaseReminderExtension(reminder ReleaseReminder, dueAt, now time.Time, memberEmail, memberName string) (ReleaseReminder, error) {
	if dueAt.Before(now.Add(AutoReleaseGracePeriod)) {
		return reminder, fmt.Errorf("release_due_at must be at least %s in the future", AutoReleaseGracePeriod)
	}
	if reminder.AutoReleaseState == ReleaseReminderAutoReleaseStateRunning {
		return reminder, errors.New("automatic release is already running")
	}
	reminder.ReleaseDueAt = dueAt.UTC().Format(time.RFC3339)
	reminder.LastExtendedByEmail = memberEmail
	reminder.LastExtendedByName = memberName
	reminder.LastExtendedAt = now.UTC().Format(time.RFC3339)
	reminder.Status = ReleaseReminderStatusActive
	reminder.LastNotifiedAt = ""
	reminder.AutoReleaseAt = ""
	reminder.AutoReleaseStartedAt = ""
	reminder.AutoReleaseLastAttemptAt = ""
	reminder.AutoReleaseAttempts = 0
	reminder.AutoReleaseLastError = ""
	reminder.AutoReleaseState = ""
	return reminder, nil
}

var errAutoReleaseCycleChanged = errors.New("automatic release cycle changed")

func (c *AutoReleaseCoordinator) Scan(ctx context.Context) error {
	if err := c.validate(); err != nil {
		return err
	}
	now := c.Now().UTC()
	reminders, err := c.Store.ListReleaseReminders("")
	if err != nil {
		return err
	}
	var scanErr error
	for _, reminder := range reminders {
		if err := c.scanReminder(ctx, reminder, now); err != nil && !errors.Is(err, errAutoReleaseCycleChanged) {
			scanErr = errors.Join(scanErr, fmt.Errorf("auto release %s: %w", reminder.ProfileName, err))
		}
	}
	return scanErr
}

func (c *AutoReleaseCoordinator) validate() error {
	if c.Now == nil || c.Store == nil || c.Jobs == nil || c.ResolveProfile == nil || c.Status == nil || c.StartDestroy == nil {
		return errors.New("automatic release coordinator dependencies are incomplete")
	}
	return nil
}

func (c *AutoReleaseCoordinator) scanReminder(ctx context.Context, reminder ReleaseReminder, now time.Time) error {
	if reminder.Status == ReleaseReminderStatusActive {
		return c.scheduleDue(reminder, now)
	}
	if reminder.Status == ReleaseReminderStatusReleased || !reminder.AutoReleaseEnabled {
		return nil
	}
	switch reminder.AutoReleaseState {
	case ReleaseReminderAutoReleaseStateScheduled, ReleaseReminderAutoReleaseStateRetrying:
		return c.advancePending(ctx, reminder, now)
	case ReleaseReminderAutoReleaseStateRunning:
		return c.observeRunning(ctx, reminder, now)
	case ReleaseReminderAutoReleaseStateNotifying:
		return c.observeNotificationPending(ctx, reminder, now)
	default:
		return nil
	}
}

func (c *AutoReleaseCoordinator) scheduleDue(reminder ReleaseReminder, now time.Time) error {
	dueAt, err := parseAutoReleaseTime(reminder.ReleaseDueAt)
	if err != nil || dueAt.After(now) {
		return nil
	}
	if c.Notify != nil {
		if err := c.Notify(AutoReleaseNotification{Kind: AutoReleaseNotificationDue, Reminder: reminder}); err != nil {
			return err
		}
	}
	updated, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if current.Status != ReleaseReminderStatusActive || current.ReleaseDueAt != reminder.ReleaseDueAt {
			return current, errAutoReleaseCycleChanged
		}
		current.Status = ReleaseReminderStatusDueNotified
		current.LastNotifiedAt = now.Format(time.RFC3339)
		if current.AutoReleaseEnabled {
			current.AutoReleaseAt = now.Add(AutoReleaseGracePeriod).Format(time.RFC3339)
			current.AutoReleaseState = ReleaseReminderAutoReleaseStateScheduled
		}
		return current, nil
	})
	if err == nil {
		c.emit("scheduled", updated, 0, updated.AutoReleaseAt)
	}
	return err
}

func (c *AutoReleaseCoordinator) advancePending(ctx context.Context, reminder ReleaseReminder, now time.Time) error {
	autoAt, err := parseAutoReleaseTime(reminder.AutoReleaseAt)
	if err != nil {
		return c.finishFailure(reminder, now, TerminalAutoReleaseError(fmt.Errorf("invalid automatic release schedule: %w", err)))
	}
	if reminder.AutoReleaseState == ReleaseReminderAutoReleaseStateScheduled && now.Before(autoAt) {
		return nil
	}
	if reminder.AutoReleaseState == ReleaseReminderAutoReleaseStateRetrying {
		startedAt, err := parseAutoReleaseTime(reminder.AutoReleaseStartedAt)
		if err != nil {
			return c.finishFailure(reminder, now, TerminalAutoReleaseError(fmt.Errorf("invalid automatic release start time: %w", err)))
		}
		if !now.Before(startedAt.Add(AutoReleaseRetryWindow)) {
			return c.finishFailure(reminder, now, fmt.Errorf("automatic release retry window of %s expired", AutoReleaseRetryWindow))
		}
		lastAttempt, err := parseAutoReleaseTime(reminder.AutoReleaseLastAttemptAt)
		if err == nil && now.Before(lastAttempt.Add(AutoReleaseRetryInterval)) {
			return nil
		}
	}
	active, err := c.Jobs.Active()
	if err != nil {
		return err
	}
	if hasActiveDestroyJob(active, reminder.ProfileName) {
		return nil
	}
	claimed, err := c.claim(reminder, now)
	if err != nil {
		return err
	}
	c.emit("attempt", claimed, claimed.AutoReleaseAttempts, "automatic release claimed")
	profile, err := c.resolveAndValidateProfile(ctx, claimed)
	if err != nil {
		return c.recordAttemptFailure(claimed, now, err, false)
	}
	status, err := c.Status(ctx, profile)
	if err != nil {
		return c.recordAttemptFailure(claimed, now, err, false)
	}
	if err := validateAutoReleaseOwnership(claimed, profile, status); err != nil {
		return c.recordAttemptFailure(claimed, now, TerminalAutoReleaseError(err), true)
	}
	if autoReleaseResourcesClean(status) {
		return c.completeRelease(claimed, profile, now)
	}
	claimed, err = c.recheckBeforeDestroy(claimed, profile)
	if err != nil {
		return err
	}
	job, err := c.StartDestroy(ctx, profile)
	if err != nil {
		return c.recordAttemptFailure(claimed, now, err, true)
	}
	if job.Type != "aws-destroy" || job.Profile != claimed.ProfileName || (job.AppleEmail != "" && strings.TrimSpace(job.AppleEmail) != strings.TrimSpace(claimed.AppleEmail)) {
		return c.recordAttemptFailure(claimed, now, TerminalAutoReleaseError(fmt.Errorf("started destroy job identity does not match reminder")), true)
	}
	c.emit("started", claimed, claimed.AutoReleaseAttempts, job.ID)
	return nil
}

func (c *AutoReleaseCoordinator) recheckBeforeDestroy(claimed ReleaseReminder, profile Profile) (ReleaseReminder, error) {
	active, err := c.Jobs.Active()
	if err != nil {
		return ReleaseReminder{}, err
	}
	if hasActiveDestroyJob(active, claimed.ProfileName) {
		return ReleaseReminder{}, errAutoReleaseCycleChanged
	}
	return c.recheckClaim(claimed, profile)
}

func (c *AutoReleaseCoordinator) recheckClaim(claimed ReleaseReminder, profile Profile) (ReleaseReminder, error) {
	return c.Store.UpdateReleaseReminder(claimed.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, claimed) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateRunning || current.ReleasedAt != "" || current.ProfileName != profile.Name || strings.TrimSpace(current.AppleEmail) != strings.TrimSpace(profile.AWS.AccountEmail) {
			return current, errAutoReleaseCycleChanged
		}
		return current, nil
	})
}

func (c *AutoReleaseCoordinator) claim(reminder ReleaseReminder, now time.Time) (ReleaseReminder, error) {
	return c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseCycle(current, reminder) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != reminder.AutoReleaseState {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
		if current.AutoReleaseStartedAt == "" {
			current.AutoReleaseStartedAt = now.Format(time.RFC3339)
		}
		current.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
		current.AutoReleaseAttempts++
		current.AutoReleaseLastError = ""
		return current, nil
	})
}

func (c *AutoReleaseCoordinator) observeRunning(ctx context.Context, reminder ReleaseReminder, now time.Time) error {
	active, err := c.Jobs.Active()
	if err != nil {
		return err
	}
	if hasActiveDestroyJob(active, reminder.ProfileName) {
		return nil
	}
	jobs, err := c.Jobs.List()
	if err != nil {
		return err
	}
	job, found := latestDestroyJob(jobs, reminder)
	if !found {
		return c.markRetrying(reminder, now, errors.New("automatic release was running but no active destroy job remains"))
	}
	c.emit("job-observed", reminder, reminder.AutoReleaseAttempts, fmt.Sprintf("job_id=%s status=%s", job.ID, job.Status))
	profile, err := c.resolveAndValidateProfile(ctx, reminder)
	if err != nil {
		return c.recordAttemptFailure(reminder, now, err, false)
	}
	status, err := c.Status(ctx, profile)
	if err != nil {
		return c.recordAttemptFailure(reminder, now, err, false)
	}
	if err := validateAutoReleaseOwnership(reminder, profile, status); err != nil {
		return c.recordAttemptFailure(reminder, now, TerminalAutoReleaseError(err), true)
	}
	if autoReleaseResourcesClean(status) {
		return c.completeRelease(reminder, profile, now)
	}
	detail := strings.TrimSpace(job.LastError)
	if detail == "" {
		detail = fmt.Sprintf("destroy job %s completed as %s while managed resources remain", job.ID, job.Status)
	}
	cause := error(errors.New(detail))
	switch job.ErrorCategory {
	case JobErrorCategoryTerminal:
		cause = TerminalAutoReleaseError(cause)
	case JobErrorCategoryRecoverable:
		cause = RecoverableAutoReleaseError(cause)
	default:
		if job.Status == JobStatusDeferred || job.Status == JobStatusSuccess {
			cause = RecoverableAutoReleaseError(cause)
		}
	}
	return c.recordAttemptFailure(reminder, now, cause, true)
}

func (c *AutoReleaseCoordinator) resolveAndValidateProfile(ctx context.Context, reminder ReleaseReminder) (Profile, error) {
	profile, err := c.ResolveProfile(ctx, reminder)
	if err != nil {
		return Profile{}, err
	}
	if profile.Name != reminder.ProfileName {
		return Profile{}, TerminalAutoReleaseError(fmt.Errorf("resolved profile %q does not match stored profile %q", profile.Name, reminder.ProfileName))
	}
	if strings.TrimSpace(reminder.AppleEmail) == "" || strings.TrimSpace(profile.AWS.AccountEmail) != strings.TrimSpace(reminder.AppleEmail) {
		return Profile{}, TerminalAutoReleaseError(fmt.Errorf("apple account mismatch: stored=%q profile=%q", reminder.AppleEmail, profile.AWS.AccountEmail))
	}
	return profile, nil
}

func (c *AutoReleaseCoordinator) recordAttemptFailure(reminder ReleaseReminder, now time.Time, cause error, safetyChecked bool) error {
	category := autoReleaseErrorCategoryOf(cause)
	if category == autoReleaseErrorTerminal || (category == autoReleaseErrorUnknown && !safetyChecked) {
		return c.finishFailure(reminder, now, cause)
	}
	startedAt, err := parseAutoReleaseTime(reminder.AutoReleaseStartedAt)
	if err == nil && !now.Before(startedAt.Add(AutoReleaseRetryWindow)) {
		return c.finishFailure(reminder, now, cause)
	}
	return c.markRetrying(reminder, now, cause)
}

func (c *AutoReleaseCoordinator) markRetrying(reminder ReleaseReminder, now time.Time, cause error) error {
	updated, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, reminder) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateRunning {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
		current.AutoReleaseLastError = cause.Error()
		return current, nil
	})
	if err != nil {
		return err
	}
	c.emit("retrying", updated, updated.AutoReleaseAttempts, cause.Error())
	if updated.AutoReleaseAttempts == 1 && c.Notify != nil {
		return c.Notify(AutoReleaseNotification{Kind: AutoReleaseNotificationFirstFailure, Reminder: updated, Error: cause.Error()})
	}
	return nil
}

func (c *AutoReleaseCoordinator) finishFailure(reminder ReleaseReminder, now time.Time, cause error) error {
	updated, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, reminder) || current.Status == ReleaseReminderStatusReleased || !current.AutoReleaseEnabled {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseState = ReleaseReminderAutoReleaseStateFailed
		current.AutoReleaseLastError = cause.Error()
		return current, nil
	})
	if err != nil {
		return err
	}
	c.emit("failed", updated, updated.AutoReleaseAttempts, cause.Error())
	if c.Notify != nil {
		return c.Notify(AutoReleaseNotification{Kind: AutoReleaseNotificationFinalFailure, Reminder: updated, Error: cause.Error()})
	}
	return nil
}

func (c *AutoReleaseCoordinator) completeRelease(reminder ReleaseReminder, profile Profile, now time.Time) error {
	checked, err := c.recheckClaim(reminder, profile)
	if err != nil {
		return err
	}
	pending, err := c.Store.UpdateReleaseReminder(checked.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, checked) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateRunning {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseState = ReleaseReminderAutoReleaseStateNotifying
		current.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
		current.AutoReleaseLastError = ""
		return current, nil
	})
	if err != nil {
		return err
	}
	c.emit("notification-pending", pending, pending.AutoReleaseAttempts, "resources_clean=true eip_retained=true")
	return c.notifyAndFinalizeRelease(pending, now)
}

func (c *AutoReleaseCoordinator) observeNotificationPending(ctx context.Context, reminder ReleaseReminder, now time.Time) error {
	lastAttempt, err := parseAutoReleaseTime(reminder.AutoReleaseLastAttemptAt)
	if err == nil && now.Before(lastAttempt.Add(AutoReleaseRetryInterval)) {
		return nil
	}
	profile, err := c.resolveAndValidateProfile(ctx, reminder)
	if err != nil {
		return c.recordPendingCompletionFailure(reminder, now, err)
	}
	status, err := c.Status(ctx, profile)
	if err != nil {
		return c.recordPendingCompletionFailure(reminder, now, err)
	}
	if err := validateAutoReleaseOwnership(reminder, profile, status); err != nil {
		return c.recordPendingCompletionFailure(reminder, now, err)
	}
	if !autoReleaseResourcesClean(status) {
		cause := RecoverableAutoReleaseError(errors.New("managed resources reappeared before automatic release completion"))
		if reminder.AutoReleaseNotifiedAt == "" {
			return c.resumeReleaseRetrying(reminder, now, cause)
		}
		return c.recordCleanupFailure(reminder, now, cause)
	}
	claimed, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, reminder) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
		current.AutoReleaseLastError = ""
		return current, nil
	})
	if err != nil {
		return err
	}
	return c.notifyAndFinalizeRelease(claimed, now)
}

func (c *AutoReleaseCoordinator) notifyAndFinalizeRelease(reminder ReleaseReminder, now time.Time) error {
	if reminder.AutoReleaseNotifiedAt == "" {
		if c.Notify != nil {
			if err := c.Notify(AutoReleaseNotification{Kind: AutoReleaseNotificationSuccess, Reminder: reminder}); err != nil {
				return c.recordNotificationFailure(reminder, now, err)
			}
		}
		marked, err := c.Store.MarkAutoReleaseNotified(releaseReminderCycleFromReminder(reminder), now.Format(time.RFC3339))
		if err != nil {
			ambiguous := fmt.Errorf("automatic release success notification was accepted but marker persistence is ambiguous; notification may be duplicated on retry: %w", err)
			c.emit("notification-persistence-ambiguous", reminder, reminder.AutoReleaseAttempts, ambiguous.Error())
			return ambiguous
		}
		reminder = marked
	}
	updated, err := c.Store.CompleteAutoRelease(releaseReminderCycleFromReminder(reminder), now.Format(time.RFC3339))
	if err != nil {
		return c.recordCleanupFailure(reminder, now, fmt.Errorf("cleanup released profile records: %w", err))
	}
	c.emit("released", updated, updated.AutoReleaseAttempts, "eip_retained=true notification=sent")
	return nil
}

func (c *AutoReleaseCoordinator) recordNotificationFailure(reminder ReleaseReminder, now time.Time, cause error) error {
	return c.recordPendingFailure(reminder, now, cause, false)
}

func (c *AutoReleaseCoordinator) recordCleanupFailure(reminder ReleaseReminder, now time.Time, cause error) error {
	if err := c.recordPendingFailure(reminder, now, cause, true); err != nil {
		return err
	}
	return RecoverableAutoReleaseError(cause)
}

func (c *AutoReleaseCoordinator) recordPendingCompletionFailure(reminder ReleaseReminder, now time.Time, cause error) error {
	if reminder.AutoReleaseNotifiedAt != "" {
		return c.recordCleanupFailure(reminder, now, cause)
	}
	return c.recordNotificationFailure(reminder, now, cause)
}

func (c *AutoReleaseCoordinator) recordPendingFailure(reminder ReleaseReminder, now time.Time, cause error, notified bool) error {
	updated, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseCycle(current, reminder) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying {
			return current, errAutoReleaseCycleChanged
		}
		if notified && current.AutoReleaseNotifiedAt == "" {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
		current.AutoReleaseLastError = cause.Error()
		return current, nil
	})
	if err != nil {
		return err
	}
	action := "notification-retrying"
	if notified {
		action = "cleanup-retrying"
	}
	c.emit(action, updated, updated.AutoReleaseAttempts, cause.Error())
	return nil
}

func (c *AutoReleaseCoordinator) resumeReleaseRetrying(reminder ReleaseReminder, now time.Time, cause error) error {
	updated, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, reminder) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying || current.AutoReleaseNotifiedAt != "" {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseState = ReleaseReminderAutoReleaseStateRetrying
		current.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
		current.AutoReleaseLastError = cause.Error()
		return current, nil
	})
	if err != nil {
		return err
	}
	c.emit("retrying", updated, updated.AutoReleaseAttempts, cause.Error())
	return nil
}

func releaseReminderCycleFromReminder(reminder ReleaseReminder) ReleaseReminderCycle {
	return ReleaseReminderCycle{
		ProfileName:          reminder.ProfileName,
		AutoReleaseAt:        reminder.AutoReleaseAt,
		AutoReleaseStartedAt: reminder.AutoReleaseStartedAt,
		HostID:               reminder.HostID,
		AppleEmail:           reminder.AppleEmail,
		OwnerEmail:           reminder.OwnerEmail,
	}
}

func (c *AutoReleaseCoordinator) emit(action string, reminder ReleaseReminder, attempt int, message string) {
	if c.Emit != nil {
		c.Emit(AutoReleaseEvent{Action: action, Reminder: reminder, Attempt: attempt, Message: message})
	}
}

func parseAutoReleaseTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, errors.New("timestamp is empty")
	}
	return time.Parse(time.RFC3339, value)
}

func sameAutoReleaseCycle(a, b ReleaseReminder) bool {
	return a.ProfileName == b.ProfileName && a.AutoReleaseAt == b.AutoReleaseAt && a.ReleaseDueAt == b.ReleaseDueAt
}

func sameAutoReleaseClaim(a, b ReleaseReminder) bool {
	return sameAutoReleaseCycle(a, b) &&
		a.AppleEmail == b.AppleEmail &&
		a.AutoReleaseStartedAt == b.AutoReleaseStartedAt &&
		a.AutoReleaseLastAttemptAt == b.AutoReleaseLastAttemptAt &&
		a.AutoReleaseAttempts == b.AutoReleaseAttempts &&
		a.LastExtendedAt == b.LastExtendedAt
}

func hasActiveDestroyJob(jobs []Job, profile string) bool {
	for _, job := range jobs {
		if job.Type == "aws-destroy" && job.Profile == profile && (job.Status == JobStatusStarting || job.Status == JobStatusRunning) {
			return true
		}
	}
	return false
}

func latestDestroyJob(jobs []Job, reminder ReleaseReminder) (Job, bool) {
	lastAttempt, _ := parseAutoReleaseTime(reminder.AutoReleaseLastAttemptAt)
	var latest Job
	found := false
	for _, job := range jobs {
		if job.Type != "aws-destroy" || job.Profile != reminder.ProfileName || (job.AppleEmail != "" && strings.TrimSpace(job.AppleEmail) != strings.TrimSpace(reminder.AppleEmail)) || (!lastAttempt.IsZero() && job.StartedAt.Before(lastAttempt)) {
			continue
		}
		if !found || job.StartedAt.After(latest.StartedAt) {
			latest, found = job, true
		}
	}
	return latest, found
}

func autoReleaseResourcesClean(status AWSStatus) bool {
	return len(status.Hosts) == 0 && len(status.Instances) == 0 && strings.TrimSpace(status.ElasticIP.AssociationID) == "" && strings.TrimSpace(status.ElasticIP.InstanceID) == ""
}

func validateAutoReleaseOwnership(reminder ReleaseReminder, profile Profile, status AWSStatus) error {
	plan := MacPlan{ProfileName: profile.Name, AccountEmail: profile.AWS.AccountEmail}
	if len(status.Hosts) > 1 || len(status.Instances) > 1 {
		return fmt.Errorf("ambiguous resource ownership: expected at most one managed host and instance")
	}
	for _, host := range status.Hosts {
		if !managedTagsMatch(host.Tags, plan) {
			return fmt.Errorf("host %s required safety tags do not match: %s", host.HostID, managedTagsMismatch(host.Tags, plan))
		}
	}
	for _, instance := range status.Instances {
		if !managedTagsMatch(instance.Tags, plan) {
			return fmt.Errorf("instance %s required safety tags do not match: %s", instance.InstanceID, managedTagsMismatch(instance.Tags, plan))
		}
	}
	if strings.TrimSpace(reminder.HostID) == "" || len(status.Hosts) == 0 {
		return nil
	}
	for _, host := range status.Hosts {
		if host.HostID == reminder.HostID {
			return nil
		}
	}
	return fmt.Errorf("ambiguous resource ownership: managed host does not match reminder host %s", reminder.HostID)
}

func autoReleaseErrorCategoryOf(err error) autoReleaseErrorCategory {
	var categorized categorizedAutoReleaseError
	if errors.As(err, &categorized) {
		return categorized.category
	}
	return autoReleaseErrorUnknown
}

type autoReleaseAPIError interface {
	error
	ErrorCode() string
}

func classifyAWSAutoReleaseError(err error) error {
	if err == nil || autoReleaseErrorCategoryOf(err) != autoReleaseErrorUnknown {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrJobsDraining) {
		return RecoverableAutoReleaseError(err)
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return RecoverableAutoReleaseError(err)
	}
	var partial AWSDestroyPartialError
	if errors.As(err, &partial) {
		return RecoverableAutoReleaseError(err)
	}
	var safety AWSSafetyError
	if errors.As(err, &safety) {
		return TerminalAutoReleaseError(err)
	}
	var apiError autoReleaseAPIError
	if !errors.As(err, &apiError) {
		return err
	}
	switch apiError.ErrorCode() {
	case "RequestLimitExceeded", "Throttling", "ThrottlingException", "ServiceUnavailable", "ServiceUnavailableException", "InternalError", "InternalFailure", "RequestTimeout", "RequestTimeoutException", "PriorRequestNotComplete":
		return RecoverableAutoReleaseError(err)
	case "AccessDenied", "AccessDeniedException", "AuthFailure", "UnauthorizedOperation", "UnrecognizedClientException", "ExpiredToken", "ExpiredTokenException", "InvalidClientTokenId", "InvalidSignatureException", "SignatureDoesNotMatch", "ValidationError", "ValidationException", "InvalidParameter", "InvalidParameterValue":
		return TerminalAutoReleaseError(err)
	default:
		return err
	}
}

func autoReleaseJobOutcome(err error, safetyChecked bool, code string) JobOutcome {
	if err == nil {
		return JobOutcome{}
	}
	err = classifyAWSAutoReleaseError(err)
	category := autoReleaseErrorCategoryOf(err)
	if category == autoReleaseErrorUnknown && !safetyChecked {
		category = autoReleaseErrorTerminal
	}
	jobCategory := JobErrorCategoryUnknown
	switch category {
	case autoReleaseErrorRecoverable:
		jobCategory = JobErrorCategoryRecoverable
	case autoReleaseErrorTerminal:
		jobCategory = JobErrorCategoryTerminal
	}
	if code == "" {
		var apiError autoReleaseAPIError
		if errors.As(err, &apiError) {
			code = apiError.ErrorCode()
		}
		var safety AWSSafetyError
		if errors.As(err, &safety) {
			code = "resource_safety"
		}
	}
	return JobOutcome{ErrorCategory: jobCategory, ErrorCode: code, Reason: redactWechatWebhookURL(err.Error())}
}
