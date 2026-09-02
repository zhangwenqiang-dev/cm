package connectmac

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMacHostArchitecture(t *testing.T) {
	for instanceType := range SupportedMacInstanceTypes {
		want := "arm64"
		if instanceType == "mac1.metal" {
			want = "x86"
		}
		if got := MacHostArchitecture(instanceType); got != want {
			t.Errorf("MacHostArchitecture(%q) = %q, want %q", instanceType, got, want)
		}
	}
	for _, instanceType := range []string{"", "unknown.metal", " mac1.metal.invalid "} {
		if got := MacHostArchitecture(instanceType); got != "" {
			t.Errorf("MacHostArchitecture(%q) = %q, want empty", instanceType, got)
		}
	}
}

func TestBuildMacPlanSelectsARMDefault(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.InstanceTypePriority = nil
	profile.AWS.AllowIntelFallback = false
	plan, err := BuildMacPlan(profile, time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildMacPlan returned error: %v", err)
	}
	if plan.ResourceName != "xcode-user@example.com" {
		t.Fatalf("resource name = %q", plan.ResourceName)
	}
	if plan.SelectedInstanceType != "mac2.metal" {
		t.Fatalf("selected instance type = %q, want mac2.metal", plan.SelectedInstanceType)
	}
	if plan.SelectedAMI != "ami-063755aadeb97329a" {
		t.Fatalf("selected ami = %q, want arm ami", plan.SelectedAMI)
	}
	for _, instanceType := range plan.InstanceTypePriority {
		if instanceType == "mac1.metal" {
			t.Fatal("default plan should exclude mac1.metal when intel fallback is disabled")
		}
	}
}

func TestBuildMacPlanSelectsIntelAMIWhenAllowed(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.AllowIntelFallback = true
	profile.AWS.InstanceTypePriority = []string{"mac1.metal"}
	plan, err := BuildMacPlan(profile, time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildMacPlan returned error: %v", err)
	}
	if plan.SelectedInstanceType != "mac1.metal" {
		t.Fatalf("selected instance type = %q, want mac1.metal", plan.SelectedInstanceType)
	}
	if plan.SelectedAMI != "ami-0538568e5d3653bea" {
		t.Fatalf("selected ami = %q, want x86 ami", plan.SelectedAMI)
	}
}

func TestBuildMacPlanCopiesSubnetsByAZ(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.SubnetsByAZ = map[string]string{"usw2-az1": "<subnet-id-az1>"}
	plan, err := BuildMacPlan(profile, time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildMacPlan returned error: %v", err)
	}
	if plan.SubnetForAZ("usw2-az1") != "<subnet-id-az1>" {
		t.Fatalf("subnet for az = %q", plan.SubnetForAZ("usw2-az1"))
	}
	profile.AWS.SubnetsByAZ["usw2-az1"] = "changed"
	if plan.SubnetForAZ("usw2-az1") != "<subnet-id-az1>" {
		t.Fatalf("plan should copy subnet map, got %q", plan.SubnetForAZ("usw2-az1"))
	}
}

func TestFormatMacPlanIncludesSafetyDetails(t *testing.T) {
	plan, err := BuildMacPlan(validAWSProfile(), time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	text := FormatMacPlan(plan)
	for _, want := range []string{
		"AutoPlacement=off",
		"HostMaintenance=off",
		"cm-managed=true",
		"Elastic IP owner tag: Apple=user@example.com",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plan text missing %q:\n%s", want, text)
		}
	}
}

func TestAWSServiceStatusUsesClient(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			CallerIdentity: CallerIdentity{Account: "123456789012", ARN: "arn:aws:iam::123456789012:user/cm"},
			Hosts:          []DedicatedHostStatus{{HostID: "h-1", State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{
				InstanceID:          "i-1",
				State:               "running",
				SystemStatus:        "ok",
				InstanceStatusCheck: "ok",
				EBSStatus:           "ok",
			}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", PublicIP: "203.0.113.10", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
		},
	}
	service := testAWSService(fake)
	plan, status, err := service.Status(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if plan.ResourceName != "xcode-user@example.com" {
		t.Fatalf("resource name = %q", plan.ResourceName)
	}
	if len(status.Hosts) != 1 || status.Hosts[0].HostID != "h-1" {
		t.Fatalf("unexpected hosts: %+v", status.Hosts)
	}
	text := FormatAWSStatus(plan, status)
	for _, want := range []string{"system_status=ok", "instance_status=ok", "ebs_status=ok", "Ready: false"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status text missing %q:\n%s", want, text)
		}
	}
}

func TestAWSServiceCapacityCombinesQuotasUsageAndOfferings(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.InstanceTypePriority = []string{"mac2-m2.metal", "mac2.metal", "mac1.metal"}
	profile.AWS.AllowIntelFallback = true
	fake := &fakeAWSClient{
		quotas: map[string]float64{
			"mac2-m2.metal": 1,
			"mac2.metal":    2,
			"mac1.metal":    1,
		},
		allHosts: []DedicatedHostStatus{
			{HostID: "h-m2", State: "available", InstanceType: "mac2-m2.metal", ZoneID: "usw2-az1"},
			{HostID: "h-mac2", State: "pending", InstanceType: "mac2.metal", ZoneID: "usw2-az2"},
			{HostID: "h-old", State: "released", InstanceType: "mac1.metal", ZoneID: "usw2-az3"},
		},
		offerings: map[string][]string{
			"mac2-m2.metal": {"usw2-az1", "usw2-az2"},
			"mac2.metal":    {"usw2-az2"},
			"mac1.metal":    {"usw2-az3"},
		},
	}
	service := testAWSService(fake)
	plan, capacity, err := service.Capacity(context.Background(), profile)
	if err != nil {
		t.Fatalf("Capacity returned error: %v", err)
	}
	text := FormatAWSCapacity(plan, capacity)
	for _, want := range []string{
		"AWS Mac capacity for profile xcode-vnc",
		"mac2-m2.metal  1      1       0",
		"mac2.metal     2      1       1",
		"mac1.metal     1      0       1",
		"Running Dedicated mac2-m2 Hosts",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("capacity output missing %q:\n%s", want, text)
		}
	}
}

func TestAWSServiceStatusHidesTerminalResourcesByDefault(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts: []DedicatedHostStatus{
				{HostID: "h-active", State: "available", Tags: managedTestTags()},
				{HostID: "h-released", State: "released", Tags: managedTestTags()},
			},
			Instances: []InstanceStatus{
				{InstanceID: "i-active", State: "running", Tags: managedTestTags()},
				{InstanceID: "i-terminated", State: "terminated", Tags: managedTestTags()},
			},
		},
	}
	service := testAWSService(fake)
	_, status, err := service.Status(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if len(status.Hosts) != 1 || status.Hosts[0].HostID != "h-active" {
		t.Fatalf("hosts = %+v", status.Hosts)
	}
	if len(status.Instances) != 1 || status.Instances[0].InstanceID != "i-active" {
		t.Fatalf("instances = %+v", status.Instances)
	}
	_, all, err := service.StatusWithOptions(context.Background(), validAWSProfile(), AWSStatusOptions{IncludeTerminal: true})
	if err != nil {
		t.Fatalf("StatusWithOptions returned error: %v", err)
	}
	if len(all.Hosts) != 2 || len(all.Instances) != 2 {
		t.Fatalf("all status = %+v", all)
	}
}

func TestAWSStatusReadyRequiresAllChecksAndEIP(t *testing.T) {
	status := AWSStatus{
		Instances: []InstanceStatus{{
			InstanceID:          "i-1",
			State:               "running",
			SystemStatus:        "ok",
			InstanceStatusCheck: "ok",
			EBSStatus:           "ok",
		}},
		ElasticIP: ElasticIP{InstanceID: "i-1"},
	}
	if !AWSStatusReady(status) {
		t.Fatalf("expected ready: %s", AWSReadinessSummary(status))
	}
	status.Instances[0].EBSStatus = ""
	if !AWSStatusReady(status) {
		t.Fatal("missing EBS status should be treated as not applicable")
	}
	status.Instances[0].EBSStatus = "initializing"
	if AWSStatusReady(status) {
		t.Fatal("initializing EBS status must not be ready")
	}
	status.Instances[0].EBSStatus = "ok"
	status.ElasticIP.InstanceID = "i-other"
	if AWSStatusReady(status) {
		t.Fatal("wrong EIP binding must not be ready")
	}
}

func TestAWSOpenAction(t *testing.T) {
	action := AWSOpenAction(AWSStatus{
		Hosts: []DedicatedHostStatus{{HostID: "h-empty", State: "available"}},
	})
	if action.Kind != "launch-on-host" || action.HostID != "h-empty" {
		t.Fatalf("action = %+v", action)
	}
	action = AWSOpenAction(AWSStatus{
		ElasticIP: ElasticIP{InstanceID: "i-other"},
	})
	if action.Kind != "blocked" {
		t.Fatalf("action = %+v", action)
	}
}

func TestFormatAWSOpenPreviewWithCandidates(t *testing.T) {
	plan := validPlan(t)
	status := AWSStatus{ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>"}}
	text := FormatAWSOpenPreviewWithCandidates(plan, status, []AWSCreateAttempt{{
		AvailabilityZoneID: "use1-az6",
		InstanceType:       "mac2-m2.metal",
		AMI:                "ami-arm",
		SubnetID:           "<subnet-id-az6>",
		Status:             "candidate",
		Detail:             "ready for AllocateHost",
	}})
	for _, want := range []string{"Decision: create", "Next: cm aws open xcode-vnc --confirm", "Guidance:", "billable Mac Dedicated Host", "Create candidates:", "use1-az6", "mac2-m2.metal", "candidate"} {
		if !strings.Contains(text, want) {
			t.Fatalf("open preview missing %q:\n%s", want, text)
		}
	}
}

func TestFormatMacDestroyPreviewIncludesGuidance(t *testing.T) {
	text := FormatMacDestroyPreviewWithStatus(validPlan(t), AWSStatus{
		ElasticIP: ElasticIP{AllocationID: "eipalloc-1", PublicIP: "54.1.2.3"},
	})
	for _, want := range []string{"Elastic IP allocation", "retained=true", "Guidance:", "Elastic IP allocation is retained", "run the same destroy command again later"} {
		if !strings.Contains(text, want) {
			t.Fatalf("destroy preview missing %q:\n%s", want, text)
		}
	}
}

func TestAWSOpenDecisionNextStep(t *testing.T) {
	cases := []struct {
		action AWSOpenDecision
		want   string
	}{
		{AWSOpenDecision{Kind: "ready"}, "cm start xcode-vnc"},
		{AWSOpenDecision{Kind: "wait-ready"}, "cm aws wait-ready xcode-vnc"},
		{AWSOpenDecision{Kind: "launch-on-host"}, "cm aws open xcode-vnc --confirm"},
		{AWSOpenDecision{Kind: "create"}, "cm aws open xcode-vnc --confirm"},
		{AWSOpenDecision{Kind: "blocked", Detail: "host h-1 is pending"}, "stop: host h-1 is pending"},
	}
	for _, tc := range cases {
		if got := AWSOpenDecisionNextStep("xcode-vnc", tc.action); got != tc.want {
			t.Fatalf("next step for %+v = %q, want %q", tc.action, got, tc.want)
		}
	}
}

func TestFormatAWSManualSetupGuideExitsSSHBeforeLocalCommands(t *testing.T) {
	text := FormatAWSManualSetupGuide(validPlan(t))
	for _, want := range []string{
		"cm ssh xcode-vnc\n  # Enter the remote Mac SSH shell",
		"sudo passwd ec2-user\n  # 输入你要设置的密码，例如：12345678\n  # 再次输入你要设置的密码，例如：12345678",
		"sudo launchctl load -w /System/Library/LaunchDaemons/com.apple.screensharing.plist\n  # Start the Screen Sharing service now.\n  exit",
		"exit\n  # Exit SSH and return to the local terminal.\n  cm start xcode-vnc",
		"cm open-vnc xcode-vnc\n  # Open macOS Screen Sharing through the local tunnel.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("manual setup guide missing %q:\n%s", want, text)
		}
	}
}

func TestHostIDsFromInstancesDedupesEmptyValues(t *testing.T) {
	got := hostIDsFromInstances([]InstanceStatus{
		{InstanceID: "i-1", HostID: "h-1"},
		{InstanceID: "i-2", HostID: ""},
		{InstanceID: "i-3", HostID: "h-1"},
		{InstanceID: "i-4", HostID: "h-2"},
	})
	if strings.Join(got, ",") != "h-1,h-2" {
		t.Fatalf("host ids = %v", got)
	}
}

func TestAWSServiceWaitReadyPollsUntilAllChecksPass(t *testing.T) {
	fake := &fakeAWSClient{
		statusSequence: []AWSStatus{
			{
				Instances: []InstanceStatus{{
					InstanceID:          "i-1",
					State:               "running",
					SystemStatus:        "ok",
					InstanceStatusCheck: "initializing",
					EBSStatus:           "",
				}},
				ElasticIP: ElasticIP{InstanceID: "i-1"},
			},
			{
				Instances: []InstanceStatus{{
					InstanceID:          "i-1",
					State:               "running",
					SystemStatus:        "ok",
					InstanceStatusCheck: "ok",
					EBSStatus:           "ok",
				}},
				ElasticIP: ElasticIP{InstanceID: "i-1"},
			},
		},
	}
	service := testAWSService(fake)
	service.ReadyPollInterval = time.Millisecond
	service.ReadyTimeout = time.Second
	var progress []string
	service.Progress = func(message string) {
		progress = append(progress, message)
	}
	_, status, err := service.WaitReady(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("WaitReady returned error: %v", err)
	}
	if !AWSStatusReady(status) {
		t.Fatalf("status not ready: %+v", status)
	}
	if strings.Join(fake.calls, ",") != "status,status" {
		t.Fatalf("calls = %v", fake.calls)
	}
	if len(progress) != 1 || !strings.Contains(progress[0], "Waiting for AWS readiness:") || !strings.Contains(progress[0], "instance_status=initializing") {
		t.Fatalf("progress = %v", progress)
	}
}

func TestAWSServiceCreateAllocatesRunsAndAssociates(t *testing.T) {
	fake := &fakeAWSClient{
		eip: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
	}
	service := testAWSService(fake)
	_, result, err := service.Create(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if result.HostID != "h-created" || result.InstanceID != "i-created" || result.AssociationID != "eipassoc-created" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.SubnetID != "<subnet-id>" {
		t.Fatalf("subnet id = %q", result.SubnetID)
	}
	calls := strings.Join(fake.calls, ",")
	for _, want := range []string{
		"verify-eip",
		"quotas:mac2.metal,mac2-m2.metal",
		"all-hosts",
		"allocate:usw2-az1:mac2.metal",
		"run:h-created:<subnet-id>:mac2.metal:ami-063755aadeb97329a",
		"status",
		"associate:<elastic-ip-allocation-id>:i-created",
	} {
		if !strings.Contains(calls, want) {
			t.Fatalf("calls missing %q: %v", want, fake.calls)
		}
	}
}

func TestAWSServiceCreateUsesSubnetByAZ(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.SubnetID = ""
	profile.AWS.SubnetsByAZ = map[string]string{
		"usw2-az1": "<subnet-id-az1>",
		"usw2-az2": "<subnet-id-az2>",
	}
	fake := &fakeAWSClient{
		eip: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
		subnetAZs: map[string]string{
			"<subnet-id-az1>": "usw2-az1",
			"<subnet-id-az2>": "usw2-az2",
		},
	}
	service := testAWSService(fake)
	_, result, err := service.Create(context.Background(), profile)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if result.SubnetID != "<subnet-id-az1>" {
		t.Fatalf("subnet id = %q", result.SubnetID)
	}
}

func TestAWSServiceCreateSkipsMismatchedSubnetAZ(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.AvailabilityZoneIDs = []string{"usw2-az1", "usw2-az2"}
	fake := &fakeAWSClient{
		eip: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
		subnetAZs: map[string]string{
			"<subnet-id>": "usw2-az2",
		},
	}
	service := testAWSService(fake)
	_, result, err := service.Create(context.Background(), profile)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if result.AvailabilityZoneID != "usw2-az2" {
		t.Fatalf("availability zone = %q", result.AvailabilityZoneID)
	}
}

func TestAWSServiceCreateSkipsUnsupportedAvailabilityZone(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.AvailabilityZoneIDs = []string{"usw2-az1", "usw2-az2"}
	profile.AWS.SubnetID = ""
	profile.AWS.SubnetsByAZ = map[string]string{
		"usw2-az1": "<subnet-id-az1>",
		"usw2-az2": "<subnet-id-az2>",
	}
	fake := &fakeAWSClient{
		eip: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
		offerings: map[string][]string{
			"mac2.metal": {"usw2-az2"},
		},
		subnetAZs: map[string]string{
			"<subnet-id-az2>": "usw2-az2",
		},
	}
	service := testAWSService(fake)
	_, result, err := service.Create(context.Background(), profile)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if result.AvailabilityZoneID != "usw2-az2" {
		t.Fatalf("availability zone = %q", result.AvailabilityZoneID)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].Status != "unsupported" || result.Attempts[1].Status != "created" {
		t.Fatalf("unexpected attempts: %+v", result.Attempts)
	}
	plan, err := service.Plan(profile)
	if err != nil {
		t.Fatal(err)
	}
	text := FormatAWSCreateResult(plan, result)
	if !strings.Contains(text, "Create attempts:") || !strings.Contains(text, "unsupported") {
		t.Fatalf("attempt table missing expected content:\n%s", text)
	}
}

func TestAWSServiceCreateSkipsMissingSubnetCandidate(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.SubnetID = ""
	profile.AWS.SubnetsByAZ = map[string]string{"usw2-az2": "<subnet-id-az2>"}
	fake := &fakeAWSClient{
		eip: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
		subnetAZs: map[string]string{
			"<subnet-id-az2>": "usw2-az2",
		},
	}
	service := testAWSService(fake)
	_, result, err := service.Create(context.Background(), profile)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if result.AvailabilityZoneID != "usw2-az2" {
		t.Fatalf("availability zone = %q", result.AvailabilityZoneID)
	}
	if len(result.Attempts) < 2 || result.Attempts[0].Status != "no-subnet" {
		t.Fatalf("unexpected attempts: %+v", result.Attempts)
	}
}

func TestAWSServiceCreateSkipsQuotaExhaustedCandidate(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.InstanceTypePriority = []string{"mac2.metal", "mac2-m2.metal"}
	fake := &fakeAWSClient{
		eip:      ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
		quotas:   map[string]float64{"mac2.metal": 1, "mac2-m2.metal": 1},
		allHosts: []DedicatedHostStatus{{HostID: "h-used", State: "available", InstanceType: "mac2.metal"}},
	}
	service := testAWSService(fake)
	_, result, err := service.Create(context.Background(), profile)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if result.InstanceType != "mac2-m2.metal" {
		t.Fatalf("instance type = %q", result.InstanceType)
	}
	if result.Attempts[0].Status != "quota-exhausted" {
		t.Fatalf("unexpected attempts: %+v", result.Attempts)
	}
	calls := strings.Join(fake.calls, ",")
	if strings.Contains(calls, "allocate:usw2-az1:mac2.metal") {
		t.Fatalf("must not allocate quota-exhausted type, calls = %v", fake.calls)
	}
}

func TestAWSServiceCreateRejectsWrongEIPOwner(t *testing.T) {
	fake := &fakeAWSClient{
		eip: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "other@example.com"}}},
	}
	service := testAWSService(fake)
	_, _, err := service.Create(context.Background(), validAWSProfile())
	if err == nil || !strings.Contains(err.Error(), "missing required owner tag") {
		t.Fatalf("expected owner tag error, got %v", err)
	}
}

func TestAWSServiceCreateStopsAfterHostAllocatedWhenEIPAssociationFails(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.InstanceTypePriority = []string{"mac2.metal", "mac2-m2.metal"}
	fake := &fakeAWSClient{
		eip:          ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
		associateErr: fmt.Errorf("instance pending"),
	}
	service := testAWSService(fake)
	_, _, err := service.Create(context.Background(), profile)
	if err == nil || !strings.Contains(err.Error(), "stop retrying") {
		t.Fatalf("expected stop retrying error, got %v", err)
	}
	if !strings.Contains(err.Error(), "Failed stage: associate-elastic-ip") || !strings.Contains(err.Error(), "Created resources: host=") {
		t.Fatalf("expected stage and resource guidance, got %v", err)
	}
	calls := strings.Join(fake.calls, ",")
	if strings.Count(calls, "allocate:") != 1 {
		t.Fatalf("expected exactly one host allocation, calls = %v", fake.calls)
	}
	if strings.Contains(calls, "allocate:usw2-az1:mac2-m2.metal") || strings.Contains(calls, "run:h-created:<subnet-id>:mac2-m2.metal") {
		t.Fatalf("must not allocate or run next instance type after host allocation, calls = %v", fake.calls)
	}
	if strings.Contains(calls, "terminate:") || strings.Contains(calls, "release:") {
		t.Fatalf("must not auto-terminate/release after association failure, calls = %v", fake.calls)
	}
}

func TestAWSServiceCreateStopsAfterHostAllocatedWhenRunInstanceFails(t *testing.T) {
	profile := validAWSProfile()
	profile.AWS.InstanceTypePriority = []string{"mac2.metal", "mac2-m2.metal"}
	fake := &fakeAWSClient{
		eip:    ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
		runErr: fmt.Errorf("run instance failed"),
	}
	service := testAWSService(fake)
	_, _, err := service.Create(context.Background(), profile)
	if err == nil || !strings.Contains(err.Error(), "stop retrying") {
		t.Fatalf("expected stop retrying error, got %v", err)
	}
	if !strings.Contains(err.Error(), "Failed stage: run-instance") || !strings.Contains(err.Error(), "Created resources: host=") {
		t.Fatalf("expected stage and resource guidance, got %v", err)
	}
	calls := strings.Join(fake.calls, ",")
	if strings.Count(calls, "allocate:") != 1 {
		t.Fatalf("expected exactly one host allocation, calls = %v", fake.calls)
	}
	if strings.Contains(calls, "allocate:usw2-az1:mac2-m2.metal") || strings.Contains(calls, "run:h-created:<subnet-id>:mac2-m2.metal") {
		t.Fatalf("must not allocate or run next instance type after host allocation, calls = %v", fake.calls)
	}
	if strings.Contains(calls, "terminate:") || strings.Contains(calls, "release:") {
		t.Fatalf("must not auto-terminate/release after run instance failure, calls = %v", fake.calls)
	}
}

func TestAWSServiceDestroyRequiresManagedTags(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", Tags: []AWSTagConfig{{Key: "Name", Value: "not-managed"}}}},
			Instances: []InstanceStatus{{InstanceID: "i-1", Tags: managedTestTags()}},
		},
	}
	service := testAWSService(fake)
	_, _, err := service.Destroy(context.Background(), validAWSProfile())
	if err == nil || !strings.Contains(err.Error(), "refuse to release host") {
		t.Fatalf("expected safety tag error, got %v", err)
	}
	for _, want := range []string{"missing cm-managed=true", "missing cm-profile=xcode-vnc", "missing cm-account-email=user@example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected missing tag detail %q, got %v", want, err)
		}
	}
}

func TestAWSServiceDestroyRunsSafeOrder(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{InstanceID: "i-1", Tags: managedTestTags()}},
			ElasticIP: ElasticIP{AssociationID: "eipassoc-1", InstanceID: "i-1"},
		},
	}
	service := testAWSService(fake)
	_, result, err := service.Destroy(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Destroy returned error: %v", err)
	}
	want := []string{"status", "disassociate:eipassoc-1", "terminate:i-1", "status", "release:h-1"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
	if len(result.TerminatedInstances) != 1 || strings.Join(result.ReleasedHosts, ",") != "h-1" || len(result.DeferredHosts) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	text := FormatAWSDestroyResult(validPlan(t), result)
	for _, want := range []string{
		"Compute release: complete",
		"Elastic IP retained: true",
		"Need rerun: false",
		"Released hosts: h-1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("destroy result missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "Retained Elastic IP") {
		t.Fatalf("destroy result missing recovery guidance:\n%s", text)
	}
}

func TestAWSServiceDestroyReportsEC2TerminationProgress(t *testing.T) {
	fake := &fakeAWSClient{
		statusSequence: []AWSStatus{
			{
				Hosts:     []DedicatedHostStatus{{HostID: "h-1", Tags: managedTestTags()}},
				Instances: []InstanceStatus{{InstanceID: "i-1", State: "running", Tags: managedTestTags()}},
				ElasticIP: ElasticIP{AssociationID: "eipassoc-1", InstanceID: "i-1"},
			},
			{
				Hosts:     []DedicatedHostStatus{{HostID: "h-1", Tags: managedTestTags()}},
				Instances: []InstanceStatus{{InstanceID: "i-1", State: "shutting-down", Tags: managedTestTags()}},
				ElasticIP: ElasticIP{},
			},
			{
				Hosts:     []DedicatedHostStatus{{HostID: "h-1", Tags: managedTestTags()}},
				Instances: []InstanceStatus{{InstanceID: "i-1", State: "terminated", Tags: managedTestTags()}},
				ElasticIP: ElasticIP{},
			},
		},
	}
	var progress []string
	service := testAWSService(fake)
	service.DestroyPollInterval = time.Millisecond
	service.DestroyTimeout = time.Second
	service.Progress = func(message string) {
		progress = append(progress, message)
	}
	_, _, err := service.Destroy(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Destroy returned error: %v", err)
	}
	if !containsString(progress, "Waiting for EC2 termination: instance=i-1 state=shutting-down elapsed=0s; retry in 1ms") {
		t.Fatalf("progress = %#v", progress)
	}
}

func TestAWSServiceDestroyDefersPendingHostReleaseFailure(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "pending", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "terminated", Tags: managedTestTags()}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>"},
		},
		releaseErr: autoReleaseTestAPIError{code: "Client.InvalidHost.Occupied", message: "host transition is still in progress"},
	}
	service := testAWSService(fake)
	service.DestroyPollInterval = time.Millisecond
	service.DestroyTimeout = 3 * time.Millisecond
	_, result, err := service.Destroy(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Destroy returned error: %v", err)
	}
	calls := strings.Join(fake.calls, ",")
	if !strings.HasPrefix(calls, "status,release:h-1,release:h-1") {
		t.Fatalf("calls = %v", fake.calls)
	}
	if len(result.DeferredHosts) != 1 || result.DeferredHosts[0].HostID != "h-1" {
		t.Fatalf("unexpected result: %+v", result)
	}
	text := FormatAWSDestroyResult(validPlan(t), result)
	for _, want := range []string{
		"Compute release: partial",
		"Need rerun: true",
		"Suggested wait: 60 minutes",
		"Deferred hosts:",
		"release was attempted after EC2 termination but AWS Mac host transition was still in progress",
		"cm aws destroy user@example.com --confirm",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("destroy result missing %q:\n%s", want, text)
		}
	}
}

func TestAWSServiceDestroyRetriesPendingHostReleaseUntilSuccess(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "pending", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "terminated", Tags: managedTestTags()}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>"},
		},
		releaseErrs: []error{autoReleaseTestAPIError{code: "Client.InvalidHost.Occupied", message: "host transition is still in progress"}, nil},
	}
	service := testAWSService(fake)
	service.DestroyPollInterval = time.Millisecond
	service.DestroyTimeout = time.Second
	_, result, err := service.Destroy(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Destroy returned error: %v", err)
	}
	want := []string{"status", "release:h-1", "release:h-1"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
	if strings.Join(result.ReleasedHosts, ",") != "h-1" || len(result.DeferredHosts) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestAWSServiceDestroyRetriesOccupiedAvailableHostUntilSuccess(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "available", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "terminated", Tags: managedTestTags()}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>"},
		},
		releaseErrs: []error{autoReleaseTestAPIError{code: "Client.InvalidHost.Occupied", message: "host is occupied"}, nil},
	}
	service := testAWSService(fake)
	service.DestroyPollInterval = time.Millisecond
	service.DestroyTimeout = time.Second
	_, result, err := service.Destroy(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Destroy returned error: %v", err)
	}
	want := []string{"status", "release:h-1", "release:h-1"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
	if strings.Join(result.ReleasedHosts, ",") != "h-1" || len(result.DeferredHosts) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestAWSServiceDestroyDoesNotRetryUnknownAvailableHostFailure(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "available", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "terminated", Tags: managedTestTags()}},
		},
		releaseErr: errors.New("access denied"),
	}
	service := testAWSService(fake)
	service.DestroyPollInterval = time.Millisecond
	service.DestroyTimeout = time.Second
	_, _, err := service.Destroy(context.Background(), validAWSProfile())
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("Destroy error = %v", err)
	}
	want := []string{"status", "release:h-1"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
}

func TestAWSServiceDestroyDoesNotRetryPendingHostAccessDenied(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "pending", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "terminated", Tags: managedTestTags()}},
		},
		releaseErr: autoReleaseTestAPIError{code: "UnauthorizedOperation", message: "access denied"},
	}
	service := testAWSService(fake)
	service.DestroyPollInterval = time.Millisecond
	service.DestroyTimeout = time.Second
	_, _, err := service.Destroy(context.Background(), validAWSProfile())
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("Destroy error = %v", err)
	}
	want := []string{"status", "release:h-1"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
}

func TestHostReleaseRetryIntervalBacksOff(t *testing.T) {
	tests := []struct {
		elapsed time.Duration
		want    time.Duration
	}{
		{elapsed: 0, want: time.Minute},
		{elapsed: 10*time.Minute - time.Second, want: time.Minute},
		{elapsed: 10 * time.Minute, want: 3 * time.Minute},
		{elapsed: 30 * time.Minute, want: 5 * time.Minute},
	}
	for _, test := range tests {
		if got := hostReleaseRetryInterval(time.Minute, test.elapsed); got != test.want {
			t.Fatalf("hostReleaseRetryInterval(1m, %s) = %s, want %s", test.elapsed, got, test.want)
		}
	}
	if got := hostReleaseRetryInterval(time.Millisecond, time.Hour); got != time.Millisecond {
		t.Fatalf("test interval changed to %s", got)
	}
}

func TestAWSServiceDestroyReleaseHostOnSecondRun(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", State: "available", Tags: managedTestTags()}},
			Instances: []InstanceStatus{{InstanceID: "i-1", State: "terminated", Tags: managedTestTags()}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>"},
		},
	}
	service := testAWSService(fake)
	_, result, err := service.Destroy(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Destroy returned error: %v", err)
	}
	want := []string{"status", "release:h-1"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
	if strings.Join(result.SkippedInstances, ",") != "i-1:terminated" || strings.Join(result.ReleasedHosts, ",") != "h-1" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestAWSServiceDestroySkipsTerminalResourcesForResume(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts: []DedicatedHostStatus{
				{HostID: "h-released", State: "released", Tags: managedTestTags()},
				{HostID: "h-active", State: "available", Tags: managedTestTags()},
			},
			Instances: []InstanceStatus{
				{InstanceID: "i-terminated", State: "terminated", Tags: managedTestTags()},
			},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", PublicIP: "203.0.113.10"},
		},
	}
	service := testAWSService(fake)
	_, result, err := service.Destroy(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Destroy returned error: %v", err)
	}
	want := []string{"status", "release:h-active"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
	if strings.Join(result.SkippedInstances, ",") != "i-terminated:terminated" {
		t.Fatalf("skipped instances = %v", result.SkippedInstances)
	}
	if strings.Join(result.SkippedHosts, ",") != "h-released:released" {
		t.Fatalf("skipped hosts = %v", result.SkippedHosts)
	}
	if result.RetainedElasticIP.AllocationID != "<elastic-ip-allocation-id>" {
		t.Fatalf("retained eip = %+v", result.RetainedElasticIP)
	}
}

func TestAWSServiceAdoptTagsMatchedResources(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", Tags: []AWSTagConfig{{Key: "Name", Value: "xcode-user@example.com"}}}},
			Instances: []InstanceStatus{{InstanceID: "i-1", HostID: "h-1", Tags: []AWSTagConfig{{Key: "Name", Value: "xcode-user@example.com"}}}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", InstanceID: "i-1", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
		},
	}
	service := testAWSService(fake)
	_, result, err := service.Adopt(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Adopt returned error: %v", err)
	}
	want := []string{"adoption", "tag:h-1,i-1"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
	if strings.Join(result.TaggedResources, ",") != "h-1,i-1" {
		t.Fatalf("tagged resources = %v", result.TaggedResources)
	}
}

func TestAWSServiceAdoptAllowsManualInstanceNameWhenEIPPointsToHostInstance(t *testing.T) {
	fake := &fakeAWSClient{
		status: AWSStatus{
			Hosts:     []DedicatedHostStatus{{HostID: "h-1", Tags: []AWSTagConfig{{Key: "Name", Value: "xcode-user@example.com"}}}},
			Instances: []InstanceStatus{{InstanceID: "i-1", HostID: "h-1", Tags: []AWSTagConfig{{Key: "Name", Value: "h-1"}}}},
			ElasticIP: ElasticIP{AllocationID: "<elastic-ip-allocation-id>", InstanceID: "i-1", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
		},
	}
	service := testAWSService(fake)
	_, result, err := service.Adopt(context.Background(), validAWSProfile())
	if err != nil {
		t.Fatalf("Adopt returned error: %v", err)
	}
	if strings.Join(result.TaggedResources, ",") != "h-1,i-1" {
		t.Fatalf("tagged resources = %v", result.TaggedResources)
	}
	text := FormatAWSAdoptionPreview(validPlan(t), fake.status)
	if !strings.Contains(text, "Instance candidates: 1") || !strings.Contains(text, "cm-creator=Xiao Chen") {
		t.Fatalf("unexpected adoption preview:\n%s", text)
	}
}

func TestAWSServiceAdoptHostTagsEmptyHost(t *testing.T) {
	fake := &fakeAWSClient{
		host: DedicatedHostStatus{HostID: "h-empty", State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1"},
	}
	service := testAWSService(fake)
	_, result, err := service.AdoptHost(context.Background(), validAWSProfile(), "h-empty")
	if err != nil {
		t.Fatalf("AdoptHost returned error: %v", err)
	}
	if strings.Join(result.TaggedResources, ",") != "h-empty" {
		t.Fatalf("tagged resources = %v", result.TaggedResources)
	}
	want := []string{"host:h-empty", "tag:h-empty"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
}

func TestAWSServiceAdoptHostRejectsNonEmptyHost(t *testing.T) {
	fake := &fakeAWSClient{
		host: DedicatedHostStatus{HostID: "h-used", State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1", InstanceIDs: []string{"i-1"}},
	}
	service := testAWSService(fake)
	_, _, err := service.AdoptHostPreview(context.Background(), validAWSProfile(), "h-used")
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected non-empty host error, got %v", err)
	}
}

func TestAWSServiceLaunchOnHostRunsInstanceAndAssociatesEIP(t *testing.T) {
	fake := &fakeAWSClient{
		host: DedicatedHostStatus{HostID: "h-empty", State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1"},
		eip:  ElasticIP{AllocationID: "<elastic-ip-allocation-id>", Tags: []AWSTagConfig{{Key: "Apple", Value: "user@example.com"}}},
	}
	service := testAWSService(fake)
	_, result, err := service.LaunchOnHost(context.Background(), validAWSProfile(), "h-empty")
	if err != nil {
		t.Fatalf("LaunchOnHost returned error: %v", err)
	}
	if result.HostID != "h-empty" || result.InstanceID != "i-created" || result.SubnetID != "<subnet-id>" {
		t.Fatalf("unexpected result: %+v", result)
	}
	want := []string{"host:h-empty", "subnet:<subnet-id>", "verify-eip", "run:h-empty:<subnet-id>:mac2.metal:ami-063755aadeb97329a", "status", "associate:<elastic-ip-allocation-id>:i-created"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
}

func TestAWSServiceLaunchOnHostPreviewRejectsPendingHost(t *testing.T) {
	fake := &fakeAWSClient{
		host: DedicatedHostStatus{HostID: "h-pending", State: "pending", InstanceType: "mac2-m2.metal", ZoneID: "usw2-az1"},
	}
	service := testAWSService(fake)
	_, _, err := service.LaunchOnHostPreview(context.Background(), validAWSProfile(), "h-pending")
	if err == nil || !strings.Contains(err.Error(), "state is pending, want available") {
		t.Fatalf("expected pending host rejection, got %v", err)
	}
}

func testAWSService(fake *fakeAWSClient) AWSService {
	return AWSService{
		Now: func() time.Time {
			return time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
		},
		NewClient: func(ctx context.Context, plan MacPlan) (AWSClient, error) {
			return fake, nil
		},
	}
}

func validPlan(t *testing.T) MacPlan {
	t.Helper()
	plan, err := BuildMacPlan(validAWSProfile(), time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func managedTestTags() []AWSTagConfig {
	return []AWSTagConfig{
		{Key: "cm-managed", Value: "true"},
		{Key: "cm-profile", Value: "xcode-vnc"},
		{Key: "cm-account-email", Value: "user@example.com"},
	}
}

type fakeAWSClient struct {
	calls          []string
	status         AWSStatus
	statusSequence []AWSStatus
	host           DedicatedHostStatus
	allHosts       []DedicatedHostStatus
	eip            ElasticIP
	quotas         map[string]float64
	offerings      map[string][]string
	subnetAZs      map[string]string
	runErr         error
	associateErr   error
	releaseErr     error
	releaseErrs    []error
}

func (c *fakeAWSClient) CallerIdentity(ctx context.Context) (CallerIdentity, error) {
	return CallerIdentity{Account: "123456789012", ARN: "arn:aws:iam::123456789012:user/cm"}, nil
}

func (c *fakeAWSClient) DescribeStatus(ctx context.Context, plan MacPlan) (AWSStatus, error) {
	c.calls = append(c.calls, "status")
	if len(c.statusSequence) > 0 {
		status := c.statusSequence[0]
		c.statusSequence = c.statusSequence[1:]
		return status, nil
	}
	return c.status, nil
}

func (c *fakeAWSClient) DescribeAdoptionCandidates(ctx context.Context, plan MacPlan) (AWSStatus, error) {
	c.calls = append(c.calls, "adoption")
	return c.status, nil
}

func (c *fakeAWSClient) DescribeHostByID(ctx context.Context, hostID string) (DedicatedHostStatus, error) {
	c.calls = append(c.calls, "host:"+hostID)
	if c.host.HostID != "" {
		return c.host, nil
	}
	return DedicatedHostStatus{HostID: hostID, State: "available", InstanceType: "mac2.metal", ZoneID: "usw2-az1"}, nil
}

func (c *fakeAWSClient) DescribeAllHosts(ctx context.Context) ([]DedicatedHostStatus, error) {
	c.calls = append(c.calls, "all-hosts")
	return c.allHosts, nil
}

func (c *fakeAWSClient) DedicatedHostQuotas(ctx context.Context, instanceTypes []string) (map[string]float64, error) {
	c.calls = append(c.calls, "quotas:"+strings.Join(instanceTypes, ","))
	return c.quotas, nil
}

func (c *fakeAWSClient) TagResources(ctx context.Context, resourceIDs []string, tags []AWSTagConfig) error {
	c.calls = append(c.calls, "tag:"+strings.Join(resourceIDs, ","))
	return nil
}

func (c *fakeAWSClient) SubnetAvailabilityZoneID(ctx context.Context, subnetID string) (string, error) {
	c.calls = append(c.calls, "subnet:"+subnetID)
	if zoneID := c.subnetAZs[subnetID]; zoneID != "" {
		return zoneID, nil
	}
	return "usw2-az1", nil
}

func (c *fakeAWSClient) InstanceTypeOfferings(ctx context.Context, instanceType string) ([]string, error) {
	c.calls = append(c.calls, "offerings:"+instanceType)
	if values := c.offerings[instanceType]; len(values) > 0 {
		return values, nil
	}
	return []string{"usw2-az1", "usw2-az2", "usw2-az3", "use2-az1", "use2-az2", "use2-az3"}, nil
}

func (c *fakeAWSClient) AllocateHost(ctx context.Context, plan MacPlan, availabilityZoneID, instanceType string) (string, error) {
	c.calls = append(c.calls, fmt.Sprintf("allocate:%s:%s", availabilityZoneID, instanceType))
	return "h-created", nil
}

func (c *fakeAWSClient) RunInstance(ctx context.Context, plan MacPlan, hostID, instanceType, amiID string) (string, error) {
	c.calls = append(c.calls, fmt.Sprintf("run:%s:%s:%s:%s", hostID, plan.SubnetID, instanceType, amiID))
	if c.runErr != nil {
		return "", c.runErr
	}
	if len(c.statusSequence) == 0 && len(c.status.Instances) == 0 {
		c.status.Instances = []InstanceStatus{{InstanceID: "i-created", State: "running", InstanceType: instanceType, HostID: hostID, Tags: managedTestTags()}}
	}
	return "i-created", nil
}

func (c *fakeAWSClient) VerifyElasticIPOwner(ctx context.Context, plan MacPlan) (ElasticIP, error) {
	c.calls = append(c.calls, "verify-eip")
	if !hasTag(c.eip.Tags, plan.ElasticIPOwnerTag.Key, plan.ElasticIPOwnerTag.Value) {
		return ElasticIP{}, fmt.Errorf("elastic ip %s is missing required owner tag %s=%s", plan.ElasticIPAllocationID, plan.ElasticIPOwnerTag.Key, plan.ElasticIPOwnerTag.Value)
	}
	return c.eip, nil
}

func (c *fakeAWSClient) AssociateElasticIP(ctx context.Context, allocationID, instanceID string) (string, error) {
	c.calls = append(c.calls, fmt.Sprintf("associate:%s:%s", allocationID, instanceID))
	if c.associateErr != nil {
		return "", c.associateErr
	}
	return "eipassoc-created", nil
}

func (c *fakeAWSClient) DisassociateElasticIP(ctx context.Context, associationID string) error {
	c.calls = append(c.calls, "disassociate:"+associationID)
	if c.status.ElasticIP.AssociationID == associationID {
		c.status.ElasticIP.AssociationID = ""
		c.status.ElasticIP.InstanceID = ""
	}
	return nil
}

func (c *fakeAWSClient) TerminateInstance(ctx context.Context, instanceID string) error {
	c.calls = append(c.calls, "terminate:"+instanceID)
	if len(c.statusSequence) == 0 {
		for i, instance := range c.status.Instances {
			if instance.InstanceID == instanceID {
				c.status.Instances[i].State = "terminated"
			}
		}
	}
	return nil
}

func (c *fakeAWSClient) ReleaseHost(ctx context.Context, hostID string) error {
	c.calls = append(c.calls, "release:"+hostID)
	if len(c.releaseErrs) > 0 {
		err := c.releaseErrs[0]
		c.releaseErrs = c.releaseErrs[1:]
		return err
	}
	if c.releaseErr != nil {
		return c.releaseErr
	}
	for i, host := range c.status.Hosts {
		if host.HostID == hostID {
			c.status.Hosts[i].State = "released"
		}
	}
	return nil
}
