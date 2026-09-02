package connectmac

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (s AWSService) Destroy(ctx context.Context, profile Profile) (MacPlan, AWSDestroyResult, error) {
	plan, err := s.Plan(profile)
	if err != nil {
		return MacPlan{}, AWSDestroyResult{}, err
	}
	client, err := s.client(ctx, plan)
	if err != nil {
		return MacPlan{}, AWSDestroyResult{}, err
	}
	status, err := client.DescribeStatus(ctx, plan)
	if err != nil {
		return MacPlan{}, AWSDestroyResult{}, err
	}
	result := AWSDestroyResult{RetainedElasticIP: status.ElasticIP}
	for _, instance := range status.Instances {
		if isTerminalInstanceState(instance.State) {
			result.SkippedInstances = append(result.SkippedInstances, fmt.Sprintf("%s:%s", instance.InstanceID, instance.State))
			continue
		}
		if !managedTagsMatch(instance.Tags, plan) {
			return MacPlan{}, AWSDestroyResult{}, AWSSafetyError{Cause: fmt.Errorf("refuse to terminate instance %s because required safety tags do not match: %s", instance.InstanceID, managedTagsMismatch(instance.Tags, plan))}
		}
		if status.ElasticIP.AssociationID != "" && status.ElasticIP.InstanceID == instance.InstanceID {
			s.progress("Disassociating Elastic IP %s from instance %s", status.ElasticIP.AssociationID, instance.InstanceID)
			if err := client.DisassociateElasticIP(ctx, status.ElasticIP.AssociationID); err != nil {
				return MacPlan{}, result, err
			}
			result.DisassociatedElasticIP = true
		}
		s.progress("Terminating EC2 instance %s and waiting for AWS termination", instance.InstanceID)
		if err := client.TerminateInstance(ctx, instance.InstanceID); err != nil {
			return MacPlan{}, result, AWSDestroyPartialError{Result: result, Cause: err}
		}
		if err := s.waitInstanceTerminated(ctx, client, plan, instance.InstanceID); err != nil {
			return MacPlan{}, result, AWSDestroyPartialError{Result: result, Cause: err}
		}
		s.progress("EC2 instance %s is terminated", instance.InstanceID)
		result.TerminatedInstances = append(result.TerminatedInstances, instance.InstanceID)
	}
	for _, host := range status.Hosts {
		if isTerminalHostState(host.State) {
			result.SkippedHosts = append(result.SkippedHosts, fmt.Sprintf("%s:%s", host.HostID, host.State))
			continue
		}
		if !managedTagsMatch(host.Tags, plan) {
			return MacPlan{}, AWSDestroyResult{}, AWSSafetyError{Cause: fmt.Errorf("refuse to release host %s because required safety tags do not match: %s", host.HostID, managedTagsMismatch(host.Tags, plan))}
		}
		released, reason, err := s.releaseHostWithRetry(ctx, client, host)
		if err != nil {
			return MacPlan{}, result, AWSDestroyPartialError{Result: result, Cause: err}
		}
		if !released {
			result.DeferredHosts = append(result.DeferredHosts, AWSDeferredHost{
				HostID: host.HostID,
				State:  emptyStatus(host.State),
				Reason: reason,
			})
			continue
		}
		result.ReleasedHosts = append(result.ReleasedHosts, host.HostID)
	}
	return plan, result, nil
}
func (s AWSService) waitInstanceTerminated(ctx context.Context, client AWSClient, plan MacPlan, instanceID string) error {
	timeout := s.DestroyTimeout
	if timeout == 0 {
		timeout = 45 * time.Minute
	}
	interval := s.DestroyPollInterval
	if interval == 0 {
		interval = 30 * time.Second
	}
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		status, err := client.DescribeStatus(ctx, plan)
		if err != nil {
			return err
		}
		instance, ok := findInstanceStatus(status, instanceID)
		if !ok || isTerminalInstanceState(instance.State) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out waiting for EC2 instance %s termination; last state=%s", instanceID, emptyStatus(instance.State))
		}
		s.progress("Waiting for EC2 termination: instance=%s state=%s elapsed=%s; retry in %s", instanceID, emptyStatus(instance.State), roundDuration(time.Since(start)), interval)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func findInstanceStatus(status AWSStatus, instanceID string) (InstanceStatus, bool) {
	for _, instance := range status.Instances {
		if instance.InstanceID == instanceID {
			return instance, true
		}
	}
	return InstanceStatus{}, false
}
func roundDuration(value time.Duration) time.Duration {
	if value < time.Second {
		return 0
	}
	return value.Round(time.Second)
}
func (s AWSService) releaseHostWithRetry(ctx context.Context, client AWSClient, host DedicatedHostStatus) (bool, string, error) {
	startedAt := time.Now()
	s.progress("Attempting to release Dedicated Host %s", host.HostID)
	err := client.ReleaseHost(ctx, host.HostID)
	if err == nil {
		s.progress("Dedicated Host %s is released", host.HostID)
		return true, "", nil
	}
	if !hostReleaseTransitionInProgress(host, err) {
		return false, "", err
	}
	timeout := s.DestroyTimeout
	if timeout == 0 {
		timeout = time.Hour
	}
	interval := s.DestroyPollInterval
	if interval == 0 {
		interval = time.Minute
	}
	deadline := startedAt.Add(timeout)
	lastErr := err
	for time.Now().Before(deadline) {
		elapsed := time.Since(startedAt)
		retryInterval := hostReleaseRetryInterval(interval, elapsed)
		s.progress("Dedicated Host cleanup is still in progress: host=%s state=%s elapsed=%s; retry release in %s", host.HostID, emptyStatus(host.State), roundDuration(elapsed), retryInterval)
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, fmt.Sprintf("release was attempted after EC2 termination but context ended while AWS Mac host transition was still in progress: %v", ctx.Err()), nil
		case <-timer.C:
		}
		err = client.ReleaseHost(ctx, host.HostID)
		if err == nil {
			s.progress("Dedicated Host %s is released", host.HostID)
			return true, "", nil
		}
		if !hostReleaseTransitionInProgress(host, err) {
			return false, "", err
		}
		lastErr = err
	}
	return false, fmt.Sprintf("release was attempted after EC2 termination but AWS Mac host transition was still in progress after %s: %v", timeout, lastErr), nil
}

func hostReleaseTransitionInProgress(host DedicatedHostStatus, err error) bool {
	if err == nil {
		return false
	}
	var apiError autoReleaseAPIError
	if errors.As(err, &apiError) {
		return apiError.ErrorCode() == "Client.InvalidHost.Occupied"
	}
	return emptyStatus(host.State) == "pending"
}

func hostReleaseRetryInterval(base, elapsed time.Duration) time.Duration {
	if base < time.Second {
		return base
	}
	if elapsed >= 30*time.Minute && base < 5*time.Minute {
		return 5 * time.Minute
	}
	if elapsed >= 10*time.Minute && base < 3*time.Minute {
		return 3 * time.Minute
	}
	return base
}
