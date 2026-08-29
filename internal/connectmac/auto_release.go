package connectmac

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	AutoReleaseGracePeriod              = 10 * time.Minute
	AutoReleaseRetryInterval            = 5 * time.Minute
	AutoReleaseRetryWindow              = time.Hour
	AutoReleaseConvergenceWindow        = 24 * time.Hour
	AutoReleaseStalledStatusInterval    = 15 * time.Minute
	AutoReleaseStalledNotificationLease = 5 * time.Minute
)

type AutoReleaseNotificationKind string

const (
	AutoReleaseNotificationDue          AutoReleaseNotificationKind = "due"
	AutoReleaseNotificationFirstFailure AutoReleaseNotificationKind = "first_failure"
	AutoReleaseNotificationFinalFailure AutoReleaseNotificationKind = "final_failure"
	AutoReleaseNotificationSuccess      AutoReleaseNotificationKind = "success"
	AutoReleaseNotificationStalled      AutoReleaseNotificationKind = "stalled"
)

type AutoReleaseNotification struct {
	Kind     AutoReleaseNotificationKind
	Reminder ReleaseReminder
	Error    string
	Attempt  int
	Retrying bool
	CycleID  string
}

type AutoReleaseEvent struct {
	Action    string
	Reminder  ReleaseReminder
	Attempt   int
	RequestID string
	JobID     string
	CycleID   string
	Message   string
}

type AutoReleaseStore interface {
	ListReleaseReminders(memberEmail string) ([]ReleaseReminder, error)
	ReleaseReminder(profileName string) (ReleaseReminder, bool, error)
	UpdateReleaseReminder(profileName string, update func(ReleaseReminder) (ReleaseReminder, error)) (ReleaseReminder, error)
	MarkAutoReleaseConvergenceAccepted(cycle ReleaseReminderCycle, acceptedAt string) (ReleaseReminder, bool, error)
	ResetLegacyAutoReleaseConvergence(cycle ReleaseReminderCycle, retryAt, reason string) (ReleaseReminder, bool, error)
	ClaimAutoReleaseStalledNotification(cycle ReleaseReminderCycle, claimedAt string, leaseDuration time.Duration) (ReleaseReminder, bool, bool, error)
	MarkAutoReleaseStalledNotified(cycle ReleaseReminderCycle, claimToken, notifiedAt string) (ReleaseReminder, bool, error)
	ReleaseAutoReleaseStalledNotificationClaim(cycle ReleaseReminderCycle, claimToken string) (ReleaseReminder, bool, error)
	ClaimAutoReleaseConvergenceStatusCheck(cycle ReleaseReminderCycle, attemptedAt string, interval time.Duration) (ReleaseReminder, bool, error)
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
	if autoReleaseStateBlocksUserMutation(reminder.AutoReleaseState) {
		return reminder, errAutomaticReleaseRunning
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
	reminder.AutoReleaseAcceptedAt = ""
	reminder.AutoReleaseStalledNotifyClaimedAt = ""
	reminder.AutoReleaseStalledNotifiedAt = ""
	reminder.AutoReleaseAttempts = 0
	reminder.AutoReleaseLastError = ""
	reminder.AutoReleaseState = ""
	return reminder, nil
}

func autoReleaseStateBlocksUserMutation(state string) bool {
	switch state {
	case ReleaseReminderAutoReleaseStateRunning,
		ReleaseReminderAutoReleaseStateRetrying,
		ReleaseReminderAutoReleaseStateNotifying:
		return true
	default:
		return false
	}
}

var (
	errAutoReleaseCycleChanged = errors.New("automatic release cycle changed")
	errAutomaticReleaseRunning = errors.New("automatic release is already running; wait for the release to finish")
)

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
			return sanitizeOperationalError(err)
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
	retryWindowExpired := false
	if reminder.AutoReleaseState == ReleaseReminderAutoReleaseStateRetrying {
		retryWindowExpired, err = autoReleaseRetryWindowExpired(reminder, now)
		if err != nil {
			return c.finishFailure(reminder, now, TerminalAutoReleaseError(fmt.Errorf("invalid automatic release start time: %w", err)))
		}
		if !retryWindowExpired {
			lastAttempt, err := parseAutoReleaseTime(reminder.AutoReleaseLastAttemptAt)
			if err == nil && now.Before(lastAttempt.Add(AutoReleaseRetryInterval)) {
				return nil
			}
		}
	}
	active, err := c.Jobs.Active()
	if err != nil {
		return err
	}
	if hasActiveDestroyJob(active, reminder.ProfileName) {
		return nil
	}
	if reminder.AutoReleaseState == ReleaseReminderAutoReleaseStateRetrying {
		handled, err := c.adoptRetryingConvergence(ctx, reminder, now)
		if err != nil || handled {
			return err
		}
	}
	if retryWindowExpired {
		jobs, err := c.Jobs.List()
		if err != nil {
			return err
		}
		completionJob, completionFound := latestDestroyJobForCompletionChecks(jobs, reminder)
		observedJob, observedFound := latestDestroyJob(jobs, reminder)
		if !observedFound && completionFound {
			observedJob, observedFound = completionJob, true
		}
		if observedFound {
			c.emitObservedJob(reminder, observedJob)
		}
		return c.inspectAtMutationDeadline(ctx, reminder, now, completionFound)
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
	if reminder.AutoReleaseAcceptedAt != "" {
		return c.observeConvergence(ctx, reminder, now)
	}
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
	retryWindowExpired, err := autoReleaseRetryWindowExpired(reminder, now)
	if err != nil {
		return c.finishFailure(reminder, now, TerminalAutoReleaseError(fmt.Errorf("invalid automatic release start time: %w", err)))
	}
	if !found {
		if retryWindowExpired {
			return c.inspectAtMutationDeadline(ctx, reminder, now, false)
		}
		return c.markRetrying(reminder, now, errors.New("automatic release was running but no active destroy job remains"))
	}
	c.emitObservedJob(reminder, job)
	profile, err := c.resolveAndValidateProfile(ctx, reminder)
	if err != nil {
		if retryWindowExpired {
			return c.recordMutationDeadlineReadFailure(reminder, now, err, autoReleaseJobSupportsCompletionChecks(job))
		}
		return c.recordAttemptFailure(reminder, now, err, false)
	}
	status, err := c.Status(ctx, profile)
	if err != nil {
		if retryWindowExpired {
			return c.recordMutationDeadlineReadFailure(reminder, now, err, autoReleaseJobSupportsCompletionChecks(job))
		}
		return c.recordAttemptFailure(reminder, now, err, false)
	}
	if err := validateAutoReleaseOwnership(reminder, profile, status); err != nil {
		return c.recordAttemptFailure(reminder, now, TerminalAutoReleaseError(err), true)
	}
	if autoReleaseResourcesClean(status) {
		return c.completeRelease(reminder, profile, now)
	}
	if acceptedReleaseConverging(reminder, job, status) {
		return c.acceptConvergence(reminder, now, job)
	}
	if retryWindowExpired {
		cause := fmt.Errorf("automatic release retry window of %s expired while managed resources remain", AutoReleaseRetryWindow)
		if autoReleaseJobSupportsCompletionChecks(job) {
			return c.markRetrying(reminder, now, cause)
		}
		return c.finishFailure(reminder, now, cause)
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

func (c *AutoReleaseCoordinator) observeConvergence(ctx context.Context, reminder ReleaseReminder, now time.Time) error {
	jobs, err := c.Jobs.List()
	if err != nil {
		return c.recordConvergenceReadFailure(reminder, fmt.Errorf("list destroy jobs for convergence evidence: %w", err))
	}
	job, found := latestDestroyJobForCompletionChecks(jobs, reminder)
	if !found || !structuredReleaseEvidenceMatches(reminder, job) {
		reason := "accepted host release evidence is missing or not structured"
		updated, transitioned, err := c.Store.ResetLegacyAutoReleaseConvergence(releaseReminderCycleFromReminder(reminder), now.UTC().Format(time.RFC3339), reason)
		if err != nil {
			return err
		}
		if transitioned {
			c.emit("convergence-evidence-invalidated", updated, updated.AutoReleaseAttempts, reason)
		}
		return nil
	}
	acceptedAt, err := parseAutoReleaseTime(reminder.AutoReleaseAcceptedAt)
	if err != nil {
		return c.recordConvergenceReadFailure(reminder, fmt.Errorf("invalid automatic release acceptance time: %w", err))
	}
	stalled := !now.Before(acceptedAt.Add(AutoReleaseConvergenceWindow))
	var warningErr error
	if stalled && reminder.AutoReleaseStalledNotifiedAt == "" {
		claimToken := now.Format(time.RFC3339)
		claimedReminder, claimed, reclaimed, err := c.Store.ClaimAutoReleaseStalledNotification(releaseReminderCycleFromReminder(reminder), claimToken, AutoReleaseStalledNotificationLease)
		if err != nil {
			warningErr = sanitizeOperationalError(fmt.Errorf("stalled convergence warning delivery claim is ambiguous: %w", err))
			c.emit("convergence-stalled-delivery-claim-ambiguous", reminder, reminder.AutoReleaseAttempts, warningErr.Error())
		} else if claimed {
			reminder = claimedReminder
			if reclaimed {
				c.emit("convergence-stalled-delivery-claim-expired-ambiguous", reminder, reminder.AutoReleaseAttempts, "expired warning delivery claim reclaimed; prior process may have delivered before stopping")
			}
			if c.Notify != nil {
				if err := c.Notify(AutoReleaseNotification{
					Kind: AutoReleaseNotificationStalled, Reminder: reminder,
					Attempt: reminder.AutoReleaseAttempts, CycleID: autoReleaseCycleID(reminder),
				}); err != nil {
					warningErr = sanitizeOperationalError(err)
					if _, _, releaseErr := c.Store.ReleaseAutoReleaseStalledNotificationClaim(releaseReminderCycleFromReminder(reminder), claimToken); releaseErr != nil {
						ambiguity := sanitizeOperationalError(fmt.Errorf("stalled warning failed and matching delivery claim release is ambiguous: %w", releaseErr))
						c.emit("convergence-stalled-delivery-claim-release-ambiguous", reminder, reminder.AutoReleaseAttempts, ambiguity.Error())
						warningErr = errors.Join(warningErr, ambiguity)
					}
				}
			}
			if warningErr == nil {
				marked, transitioned, err := c.Store.MarkAutoReleaseStalledNotified(releaseReminderCycleFromReminder(reminder), claimToken, now.Format(time.RFC3339))
				if err != nil {
					warningErr = sanitizeOperationalError(fmt.Errorf("stalled convergence warning was accepted but marker persistence is ambiguous; notification may be duplicated after lease expiry: %w", err))
					c.emit("convergence-stalled-persistence-ambiguous", reminder, reminder.AutoReleaseAttempts, warningErr.Error())
				} else {
					reminder = marked
					if transitioned {
						c.emit("convergence-stalled", reminder, reminder.AutoReleaseAttempts, "accepted host release remains incomplete after 24 hours")
					}
				}
			}
		}
	}
	if stalled {
		claimed, ok, err := c.Store.ClaimAutoReleaseConvergenceStatusCheck(releaseReminderCycleFromReminder(reminder), now.Format(time.RFC3339), AutoReleaseStalledStatusInterval)
		if err != nil {
			return errors.Join(warningErr, err)
		}
		if !ok {
			return warningErr
		}
		reminder = claimed
	}
	profile, err := c.resolveAndValidateProfile(ctx, reminder)
	if err != nil {
		if autoReleaseErrorCategoryOf(err) == autoReleaseErrorTerminal {
			return c.finishFailure(reminder, now, err)
		}
		return c.recordConvergenceReadFailure(reminder, err)
	}
	status, err := c.Status(ctx, profile)
	if err != nil {
		recordErr := c.recordConvergenceReadFailure(reminder, err)
		return errors.Join(warningErr, sanitizeOperationalError(err), recordErr)
	}
	if err := validateAutoReleaseOwnership(reminder, profile, status); err != nil {
		return errors.Join(warningErr, sanitizeOperationalError(err), c.finishFailure(reminder, now, TerminalAutoReleaseError(err)))
	}
	if autoReleaseResourcesClean(status) {
		return c.completeRelease(reminder, profile, now)
	}
	if acceptedHostReleaseTopology(reminder, status) {
		return warningErr
	}
	return errors.Join(warningErr, c.finishFailure(reminder, now, TerminalAutoReleaseError(errors.New("managed resources no longer match the accepted host release"))))
}

func (c *AutoReleaseCoordinator) recordConvergenceReadFailure(reminder ReleaseReminder, cause error) error {
	cause = sanitizeOperationalError(cause)
	updated, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, reminder) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateRunning || current.AutoReleaseAcceptedAt != reminder.AutoReleaseAcceptedAt || current.AutoReleaseAcceptedAt == "" {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseLastError = cause.Error()
		return current, nil
	})
	if err != nil {
		return err
	}
	c.emit("convergence-read-error", updated, updated.AutoReleaseAttempts, cause.Error())
	return nil
}

func (c *AutoReleaseCoordinator) adoptRetryingConvergence(ctx context.Context, reminder ReleaseReminder, now time.Time) (bool, error) {
	jobs, err := c.Jobs.List()
	if err != nil {
		return true, err
	}
	job, found := latestDestroyJobForCompletionChecks(jobs, reminder)
	if !found {
		return false, nil
	}
	c.emitObservedJob(reminder, job)
	profile, err := c.resolveAndValidateProfile(ctx, reminder)
	if err != nil {
		return true, c.finishFailure(reminder, now, err)
	}
	status, err := c.Status(ctx, profile)
	if err != nil {
		return true, c.recordExpiredCompletionCheckFailure(reminder, err)
	}
	if err := validateAutoReleaseOwnership(reminder, profile, status); err != nil {
		return true, c.finishFailure(reminder, now, TerminalAutoReleaseError(err))
	}
	if autoReleaseResourcesClean(status) {
		resumed, err := c.resumeRetryingCompletion(reminder)
		if err != nil {
			return true, err
		}
		return true, c.completeRelease(resumed, profile, now)
	}
	if acceptedReleaseConverging(reminder, job, status) {
		updated, transitioned, err := c.Store.MarkAutoReleaseConvergenceAccepted(releaseReminderCycleFromReminder(reminder), now.UTC().Format(time.RFC3339))
		if err == nil && transitioned {
			c.emitConvergenceWaiting(updated, job)
		}
		return true, err
	}
	return false, nil
}

func (c *AutoReleaseCoordinator) resumeRetryingCompletion(reminder ReleaseReminder) (ReleaseReminder, error) {
	return c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, reminder) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseState = ReleaseReminderAutoReleaseStateRunning
		current.AutoReleaseLastError = ""
		return current, nil
	})
}

func (c *AutoReleaseCoordinator) acceptConvergence(reminder ReleaseReminder, now time.Time, job Job) error {
	updated, transitioned, err := c.Store.MarkAutoReleaseConvergenceAccepted(releaseReminderCycleFromReminder(reminder), now.UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	if transitioned {
		c.emitConvergenceWaiting(updated, job)
	}
	return nil
}

func (c *AutoReleaseCoordinator) emitConvergenceWaiting(reminder ReleaseReminder, job Job) {
	if c.Emit == nil {
		return
	}
	c.Emit(AutoReleaseEvent{
		Action: "convergence-waiting", Reminder: reminder, Attempt: reminder.AutoReleaseAttempts,
		RequestID: job.RequestID, JobID: job.ID, CycleID: autoReleaseCycleID(reminder),
		Message: fmt.Sprintf("job_id=%s request_id=%s", job.ID, job.RequestID),
	})
}

func (c *AutoReleaseCoordinator) inspectAtMutationDeadline(ctx context.Context, reminder ReleaseReminder, now time.Time, completionChecksContinue bool) error {
	profile, err := c.resolveAndValidateProfile(ctx, reminder)
	if err != nil {
		return c.recordMutationDeadlineReadFailure(reminder, now, err, completionChecksContinue)
	}
	status, err := c.Status(ctx, profile)
	if err != nil {
		return c.recordMutationDeadlineReadFailure(reminder, now, err, completionChecksContinue)
	}
	if err := validateAutoReleaseOwnership(reminder, profile, status); err != nil {
		return c.finishFailure(reminder, now, TerminalAutoReleaseError(err))
	}
	if autoReleaseResourcesClean(status) {
		claimed := reminder
		if reminder.AutoReleaseState == ReleaseReminderAutoReleaseStateRetrying {
			claimed, err = c.claim(reminder, now)
			if err != nil {
				return err
			}
		}
		return c.completeRelease(claimed, profile, now)
	}
	cause := fmt.Errorf("automatic release retry window of %s expired while managed resources remain", AutoReleaseRetryWindow)
	if completionChecksContinue {
		if reminder.AutoReleaseState == ReleaseReminderAutoReleaseStateRunning {
			return c.markRetrying(reminder, now, cause)
		}
		return c.recordExpiredCompletionCheckFailure(reminder, cause)
	}
	return c.finishFailure(reminder, now, cause)
}

func (c *AutoReleaseCoordinator) recordMutationDeadlineReadFailure(reminder ReleaseReminder, now time.Time, cause error, completionChecksContinue bool) error {
	if completionChecksContinue && autoReleaseErrorCategoryOf(cause) == autoReleaseErrorRecoverable {
		if reminder.AutoReleaseState == ReleaseReminderAutoReleaseStateRunning {
			return c.markRetrying(reminder, now, cause)
		}
		return c.recordExpiredCompletionCheckFailure(reminder, cause)
	}
	return c.finishFailure(reminder, now, cause)
}

func (c *AutoReleaseCoordinator) recordExpiredCompletionCheckFailure(reminder ReleaseReminder, cause error) error {
	cause = sanitizeOperationalError(cause)
	notifyFirstFailure := false
	updated, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, reminder) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateRetrying {
			return current, errAutoReleaseCycleChanged
		}
		notifyFirstFailure = current.AutoReleaseAttempts == 1 && current.AutoReleaseLastError == ""
		current.AutoReleaseLastError = cause.Error()
		return current, nil
	})
	if err != nil {
		return err
	}
	c.emit("retrying", updated, updated.AutoReleaseAttempts, cause.Error())
	if notifyFirstFailure && c.Notify != nil {
		return sanitizeOperationalError(c.Notify(AutoReleaseNotification{Kind: AutoReleaseNotificationFirstFailure, Reminder: updated, Error: cause.Error(), Attempt: updated.AutoReleaseAttempts, CycleID: autoReleaseCycleID(updated)}))
	}
	return nil
}

func (c *AutoReleaseCoordinator) emitObservedJob(reminder ReleaseReminder, job Job) {
	if c.Emit == nil {
		return
	}
	c.Emit(AutoReleaseEvent{
		Action:    "job.observed",
		Reminder:  reminder,
		Attempt:   reminder.AutoReleaseAttempts,
		RequestID: job.RequestID,
		JobID:     job.ID,
		CycleID:   autoReleaseCycleID(reminder),
		Message:   fmt.Sprintf("job_id=%s status=%s", job.ID, job.Status),
	})
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
	cause = sanitizeOperationalError(cause)
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
		return sanitizeOperationalError(c.Notify(AutoReleaseNotification{Kind: AutoReleaseNotificationFirstFailure, Reminder: updated, Error: cause.Error(), Attempt: updated.AutoReleaseAttempts, CycleID: autoReleaseCycleID(updated)}))
	}
	return nil
}

func (c *AutoReleaseCoordinator) finishFailure(reminder ReleaseReminder, now time.Time, cause error) error {
	cause = sanitizeOperationalError(cause)
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
		return sanitizeOperationalError(c.Notify(AutoReleaseNotification{Kind: AutoReleaseNotificationFinalFailure, Reminder: updated, Error: cause.Error(), Attempt: updated.AutoReleaseAttempts, CycleID: autoReleaseCycleID(updated)}))
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
	return c.notifyAndFinalizeRelease(pending, now, false, "")
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
	if !autoReleaseResourcesClean(status) {
		return c.blockPendingCompletion(reminder, now, errors.New("managed resources reappeared before automatic release completion"))
	}
	if err := validateAutoReleaseOwnership(reminder, profile, status); err != nil {
		return c.recordPendingCompletionFailure(reminder, now, err)
	}
	retryingNotification := reminder.AutoReleaseNotifiedAt == ""
	retryError := ""
	if retryingNotification {
		retryError = reminder.AutoReleaseLastError
	}
	claimed, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, reminder) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying {
			return current, errAutoReleaseCycleChanged
		}
		if (current.AutoReleaseNotifiedAt == "") != retryingNotification {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
		current.AutoReleaseLastError = ""
		if retryingNotification {
			current.AutoReleaseAttempts++
		}
		return current, nil
	})
	if err != nil {
		return err
	}
	return c.notifyAndFinalizeRelease(claimed, now, retryingNotification, retryError)
}

func (c *AutoReleaseCoordinator) notifyAndFinalizeRelease(reminder ReleaseReminder, now time.Time, retrying bool, retryError string) error {
	if reminder.AutoReleaseNotifiedAt == "" {
		if c.Notify != nil {
			if err := c.Notify(AutoReleaseNotification{
				Kind: AutoReleaseNotificationSuccess, Reminder: reminder,
				Error: retryError, Attempt: reminder.AutoReleaseAttempts, Retrying: retrying,
				CycleID: autoReleaseCycleID(reminder),
			}); err != nil {
				return c.recordNotificationFailure(reminder, now, err)
			}
		}
		marked, err := c.Store.MarkAutoReleaseNotified(releaseReminderCycleFromReminder(reminder), now.Format(time.RFC3339))
		if err != nil {
			ambiguous := sanitizeOperationalError(fmt.Errorf("automatic release success notification was accepted but marker persistence is ambiguous; notification may be duplicated on retry: %w", err))
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
	return c.recordPendingFailure(reminder, now, sanitizeOperationalError(cause), false)
}

func (c *AutoReleaseCoordinator) recordCleanupFailure(reminder ReleaseReminder, now time.Time, cause error) error {
	cause = sanitizeOperationalError(cause)
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
	cause = sanitizeOperationalError(cause)
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
		if errors.Is(err, errAutoReleaseCycleChanged) {
			return err
		}
		return sanitizeOperationalError(err)
	}
	action := "notification-retrying"
	if notified {
		action = "cleanup-retrying"
	}
	c.emit(action, updated, updated.AutoReleaseAttempts, cause.Error())
	return nil
}

func (c *AutoReleaseCoordinator) blockPendingCompletion(reminder ReleaseReminder, now time.Time, cause error) error {
	cause = sanitizeOperationalError(cause)
	updated, err := c.Store.UpdateReleaseReminder(reminder.ProfileName, func(current ReleaseReminder) (ReleaseReminder, error) {
		if !sameAutoReleaseClaim(current, reminder) || current.Status != ReleaseReminderStatusDueNotified || !current.AutoReleaseEnabled || current.AutoReleaseState != ReleaseReminderAutoReleaseStateNotifying {
			return current, errAutoReleaseCycleChanged
		}
		current.AutoReleaseState = ReleaseReminderAutoReleaseStateFailed
		current.AutoReleaseLastAttemptAt = now.Format(time.RFC3339)
		current.AutoReleaseLastError = cause.Error()
		return current, nil
	})
	if err != nil {
		return err
	}
	c.emit("failed", updated, updated.AutoReleaseAttempts, cause.Error())
	return nil
}

func releaseReminderCycleFromReminder(reminder ReleaseReminder) ReleaseReminderCycle {
	return ReleaseReminderCycle{
		ProfileName:           reminder.ProfileName,
		AutoReleaseAt:         reminder.AutoReleaseAt,
		AutoReleaseStartedAt:  reminder.AutoReleaseStartedAt,
		AutoReleaseAcceptedAt: reminder.AutoReleaseAcceptedAt,
		HostID:                reminder.HostID,
		AppleEmail:            reminder.AppleEmail,
		OwnerEmail:            reminder.OwnerEmail,
	}
}

func autoReleaseCycleID(reminder ReleaseReminder) string {
	cycle := releaseReminderCycleFromReminder(reminder)
	digest := sha256.New()
	for _, value := range []string{
		cycle.ProfileName,
		cycle.AutoReleaseAt,
		cycle.AutoReleaseStartedAt,
		cycle.HostID,
		cycle.AppleEmail,
		cycle.OwnerEmail,
	} {
		_, _ = fmt.Fprintf(digest, "%d:", len(value))
		_, _ = digest.Write([]byte(value))
	}
	return "arc-" + hex.EncodeToString(digest.Sum(nil))
}

func (c *AutoReleaseCoordinator) emit(action string, reminder ReleaseReminder, attempt int, message string) {
	if c.Emit != nil {
		c.Emit(AutoReleaseEvent{Action: action, Reminder: reminder, Attempt: attempt, CycleID: autoReleaseCycleID(reminder), Message: message})
	}
}

func parseAutoReleaseTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, errors.New("timestamp is empty")
	}
	return time.Parse(time.RFC3339, value)
}

func autoReleaseRetryWindowExpired(reminder ReleaseReminder, now time.Time) (bool, error) {
	startedAt, err := parseAutoReleaseTime(reminder.AutoReleaseStartedAt)
	if err != nil {
		return false, err
	}
	return !now.Before(startedAt.Add(AutoReleaseRetryWindow)), nil
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

func autoReleaseJobSupportsCompletionChecks(job Job) bool {
	return job.Status == JobStatusSuccess || job.Status == JobStatusDeferred
}

func latestDestroyJobForCompletionChecks(jobs []Job, reminder ReleaseReminder) (Job, bool) {
	startedAt, _ := parseAutoReleaseTime(reminder.AutoReleaseStartedAt)
	var latest Job
	found := false
	for _, job := range jobs {
		if job.Type != "aws-destroy" ||
			job.Profile != reminder.ProfileName ||
			(job.AppleEmail != "" && strings.TrimSpace(job.AppleEmail) != strings.TrimSpace(reminder.AppleEmail)) ||
			job.StartedAt.Before(startedAt) ||
			!autoReleaseJobSupportsCompletionChecks(job) {
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

func acceptedReleaseConverging(reminder ReleaseReminder, job Job, status AWSStatus) bool {
	if !structuredReleaseEvidenceMatches(reminder, job) ||
		!acceptedHostReleaseTopology(reminder, status) {
		return false
	}
	return true
}

func structuredReleaseEvidenceMatches(reminder ReleaseReminder, job Job) bool {
	if !job.ReleaseEvidenceRecorded || !autoReleaseJobSupportsCompletionChecks(job) {
		return false
	}
	for _, releasedHostID := range job.ReleasedHosts {
		if releasedHostID == reminder.HostID {
			return true
		}
	}
	return false
}

func acceptedHostReleaseTopology(reminder ReleaseReminder, status AWSStatus) bool {
	if len(status.Instances) != 0 ||
		len(status.Hosts) != 1 ||
		strings.TrimSpace(status.ElasticIP.AssociationID) != "" ||
		strings.TrimSpace(status.ElasticIP.InstanceID) != "" {
		return false
	}

	hostID := reminder.HostID
	host := status.Hosts[0]
	if strings.TrimSpace(hostID) == "" || host.State != "pending" || host.HostID != hostID {
		return false
	}
	return true
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
	return JobOutcome{ErrorCategory: jobCategory, ErrorCode: code, Reason: sanitizeOperationalError(err).Error()}
}
